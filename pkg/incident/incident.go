// Package incident normalizes production incident payloads into a small,
// redacted context that Atteler can use for incident-to-fix workflows.
//
//nolint:wsl_v5 // Parser/redaction helpers keep related extraction branches close for readability.
package incident

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/netip"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/tommoulard/atteler/pkg/privacy"
)

const (
	// SourceSentry identifies incidents fetched from Sentry's Issues API.
	SourceSentry = "sentry"
	// SourceFile identifies incidents loaded from a local redacted JSON file.
	SourceFile = "file"
	// SourceMCP identifies incidents fetched through an MCP observability tool.
	SourceMCP = "mcp"

	// RedactionPolicyVersion records the policy used before incident context is
	// rendered into prompts, logs, reports, tests, or PR bodies.
	RedactionPolicyVersion = privacy.RedactionPolicyVersion + "+incident-pii-v3"

	redactedIncidentValue = "[REDACTED]"
	redactedEmailValue    = "[REDACTED_EMAIL]"
	redactedIPValue       = "[REDACTED_IP]"
	jsonTrueValue         = "true"

	incidentPIIKeyPattern = `(?:[a-z0-9_-]*(?:user(?:[_-]?(?:id|name))?|account[_-]?id|customer[_-]?id|tenant[_-]?id|member[_-]?id|profile[_-]?id|email|ip(?:[_-]?address)?|remote[_-]?addr|client[_-]?ip)[a-z0-9_-]*|state|oauth[_-]?state|authorization[_-]?code|auth[_-]?code|oauth[_-]?code|code[_-]?verifier|session[_-]?id|sid|jwt|nonce|saml[_-]?response)`
	// #nosec G101 -- regex key names used to find credentials; this is not a credential value.
	incidentSecretKeyPattern = `(?:[a-z0-9_-]*(?:password|passwd|pwd|api[_-]?key|authorization|auth[_-]?token|access[_-]?token|refresh[_-]?token|session[_-]?token|id[_-]?token|token|secret|private[_-]?key|user(?:[_-]?(?:id|name))?|account[_-]?id|customer[_-]?id|tenant[_-]?id|member[_-]?id|profile[_-]?id|email|ip(?:[_-]?address)?|remote[_-]?addr|client[_-]?ip)[a-z0-9_-]*|state|oauth[_-]?state|authorization[_-]?code|auth[_-]?code|oauth[_-]?code|code[_-]?verifier|session[_-]?id|sid|jwt|nonce|saml[_-]?response)`
)

var (
	emailPattern = regexp.MustCompile(`(?i)\b[A-Z0-9._%+\-]+@[A-Z0-9.\-]+\.[A-Z]{2,}\b`)
	ipv4Pattern  = regexp.MustCompile(`\b(?:\d{1,3}\.){3}\d{1,3}\b`)
	ipv6Pattern  = regexp.MustCompile(`(?i)\[?(?:[0-9a-f]{0,4}:){2,}[0-9a-f]{0,4}(?:%[a-z0-9_.-]+)?\]?`)

	incidentPIIQuotedAssignments = regexp.MustCompile(`(?i)"(` + incidentPIIKeyPattern + `)"\s*:\s*("[^"]*"|'[^']*'|[0-9]+|true|false|null)`)
	incidentPIIAssignments       = regexp.MustCompile(`(?i)\b(` + incidentPIIKeyPattern + `)(\s*[:=]\s*)("[^"]*"|'[^']*'|[^"'\\\s/?&#,;)}]+)`)

	incidentSensitiveQuotedValues = regexp.MustCompile(`(?i)"` + incidentSecretKeyPattern + `"\s*:\s*("[^"]*"|'[^']*'|[0-9]+|true|false|null)`)
	incidentSensitiveValues       = regexp.MustCompile(`(?i)\b` + incidentSecretKeyPattern + `\s*[:=]\s*("[^"]*"|'[^']*'|[^"'\\\s/?&#,;)}]+)`)
	incidentSensitiveFlagValues   = regexp.MustCompile(`(?i)(^|[\s;&|])(--?` + incidentSecretKeyPattern + `(?:\s+|=))("[^"]*"|'[^']*'|[^\s;&|]+)`)
	bearerTokenValuePattern       = regexp.MustCompile(`(?i)\bbearer\s+([A-Za-z0-9._~+\-/]+=*)`)

	jsStackFramePattern     = regexp.MustCompile(`^\s*at\s+(.+?)\s+\(([^():\s]+):(\d+)(?::(\d+))?\)\s*$`)
	jsAnonymousFramePattern = regexp.MustCompile(`^\s*at\s+([^():\s]+):(\d+)(?::(\d+))?\s*$`)
	javaStackFramePattern   = regexp.MustCompile(`^\s*at\s+(.+?)\(([^():\s]+\.java):(\d+)\)\s*$`)
	dotnetStackFramePattern = regexp.MustCompile(`^\s*at\s+(.+?)\s+in\s+(.+?\.cs):line\s+(\d+)\s*$`)
	pythonStackFramePattern = regexp.MustCompile(`^\s*File "([^"]+)", line (\d+), in (.+?)\s*$`)
	plainStackFramePattern  = regexp.MustCompile(`\b([^():\s]+):(\d+)(?::(\d+))?\b`)
)

// Context is the vendor-neutral incident shape used by the diagnosis workflow.
type Context struct {
	FirstSeen   time.Time         `json:"first_seen,omitzero"`
	LastSeen    time.Time         `json:"last_seen,omitzero"`
	Timestamp   time.Time         `json:"timestamp,omitzero"`
	Request     Request           `json:"request,omitzero"`
	Tags        map[string]string `json:"tags,omitempty"`
	Source      string            `json:"source,omitempty"`
	Reference   string            `json:"reference,omitempty"`
	URL         string            `json:"url,omitempty"`
	Service     string            `json:"service,omitempty"`
	Environment string            `json:"environment,omitempty"`
	ErrorType   string            `json:"error_type,omitempty"`
	Message     string            `json:"message,omitempty"`
	Title       string            `json:"title,omitempty"`
	Release     string            `json:"release,omitempty"`
	Version     string            `json:"version,omitempty"`
	Commit      string            `json:"commit,omitempty"`
	StackTrace  []StackFrame      `json:"stack_trace,omitempty"`
	Logs        []Observation     `json:"logs,omitempty"`
	Traces      []Observation     `json:"traces,omitempty"`
	Metrics     []Observation     `json:"metrics,omitempty"`
	Deployments []Deployment      `json:"deployments,omitempty"`
}

// Request captures request metadata without retaining sensitive payloads.
type Request struct {
	Headers  map[string]string `json:"headers,omitempty"`
	Metadata map[string]string `json:"metadata,omitempty"`
	Method   string            `json:"method,omitempty"`
	URL      string            `json:"url,omitempty"`
	Data     string            `json:"data,omitempty"`
}

// StackFrame is one normalized stack frame from an incident source.
type StackFrame struct {
	File        string `json:"file,omitempty"`
	AbsPath     string `json:"abs_path,omitempty"`
	Function    string `json:"function,omitempty"`
	Module      string `json:"module,omitempty"`
	ContextLine string `json:"context_line,omitempty"`
	Line        int    `json:"line,omitempty"`
	Column      int    `json:"column,omitempty"`
	InApp       bool   `json:"in_app,omitempty"`
}

// Observation is a normalized log, trace, metric, or breadcrumb record.
type Observation struct {
	Timestamp time.Time         `json:"timestamp,omitzero"`
	Fields    map[string]string `json:"fields,omitempty"`
	Source    string            `json:"source,omitempty"`
	Name      string            `json:"name,omitempty"`
	Message   string            `json:"message,omitempty"`
}

// Deployment captures release/deploy metadata when an integration provides it.
type Deployment struct {
	DeployedAt  time.Time `json:"deployed_at,omitzero"`
	Version     string    `json:"version,omitempty"`
	Commit      string    `json:"commit,omitempty"`
	Environment string    `json:"environment,omitempty"`
}

// ParseJSONIncident parses a vendor-neutral incident JSON payload. Sentry-like
// envelopes are delegated to ParseSentryIncident so local fixtures can exercise
// the concrete Sentry integration without making network calls.
func ParseJSONIncident(raw []byte, source, reference string) (Context, error) {
	root, err := decodeIncidentJSONPayload(raw)
	if err != nil {
		return Context{}, err
	}

	if issue, event, ok := sentryPayloadParts(root); ok {
		inc, parseErr := ParseSentryIncident(mustJSON(issue), mustJSON(event))
		if parseErr != nil {
			return Context{}, parseErr
		}

		inc.Source = firstNonEmpty(source, inc.Source, SourceSentry)
		inc.Reference = firstNonEmpty(reference, inc.Reference)

		return RedactContext(inc), nil
	}

	var inc Context
	if err := json.Unmarshal(raw, &inc); err != nil {
		// Continue with best-effort map-based extraction below. Observability
		// connectors frequently return loose shapes such as logs: ["message"],
		// which should not prevent diagnosis when the core payload is usable.
		inc = Context{}
	}

	applyGenericFields(&inc, root)

	inc.Source = firstNonEmpty(source, inc.Source)
	inc.Reference = firstNonEmpty(reference, inc.Reference)

	return RedactContext(inc), nil
}

func decodeIncidentJSONPayload(raw []byte) (map[string]any, error) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 {
		return nil, errors.New("incident: JSON payload is empty")
	}

	var value any
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if err := decoder.Decode(&value); err != nil {
		if root, ok := decodeIncidentJSONLinesPayload(raw); ok {
			return root, nil
		}

		return nil, fmt.Errorf("incident: parse JSON payload: %w", err)
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		if root, ok := decodeIncidentJSONLinesPayload(raw); ok {
			return root, nil
		}

		if err == nil {
			return nil, errors.New("incident: JSON payload must contain a single object or array")
		}

		return nil, fmt.Errorf("incident: parse JSON payload: %w", err)
	}

	switch typed := value.(type) {
	case map[string]any:
		if typed == nil {
			return nil, errors.New("incident: JSON payload must be an object or array")
		}

		return typed, nil
	case []any:
		return map[string]any{"logs": typed}, nil
	default:
		return nil, errors.New("incident: JSON payload must be an object or array")
	}
}

func decodeIncidentJSONLinesPayload(raw []byte) (map[string]any, bool) {
	text := string(bytes.TrimSpace(raw))
	if !strings.Contains(text, "\n") {
		return nil, false
	}

	logs := make([]any, 0)
	for line := range strings.Lines(text) {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "{") || strings.HasPrefix(line, "[") {
			value, ok := decodeIncidentJSONLine(line)
			if !ok {
				return nil, false
			}

			logs = append(logs, value)
			continue
		}

		logs = append(logs, line)
	}
	if len(logs) == 0 {
		return nil, false
	}

	return map[string]any{"logs": logs}, true
}

func decodeIncidentJSONLine(line string) (any, bool) {
	decoder := json.NewDecoder(strings.NewReader(line))
	decoder.UseNumber()

	var value any
	if err := decoder.Decode(&value); err != nil {
		return nil, false
	}

	return value, true
}

// RedactContext returns a defensive copy with common secrets and PII redacted.
func RedactContext(inc Context) Context {
	out := inc
	out.Source = RedactIdentifier(out.Source)
	out.Reference = RedactIdentifier(out.Reference)
	out.URL = RedactIdentifier(out.URL)
	out.Service = RedactText(out.Service)
	out.Environment = RedactText(out.Environment)
	out.ErrorType = RedactText(out.ErrorType)
	out.Message = RedactText(out.Message)
	out.Title = RedactText(out.Title)
	out.Release = RedactIdentifier(out.Release)
	out.Version = RedactIdentifier(out.Version)
	out.Commit = RedactIdentifier(out.Commit)
	out.Tags = RedactMetadata(out.Tags)
	out.Request = RedactRequest(out.Request)
	out.StackTrace = redactStackFrames(out.StackTrace)
	out.Logs = redactObservations(out.Logs)
	out.Traces = redactObservations(out.Traces)
	out.Metrics = redactObservations(out.Metrics)
	out.Deployments = redactDeployments(out.Deployments)

	return out
}

// RedactText removes common secrets and email addresses from incident text.
func RedactText(text string) string {
	if strings.TrimSpace(text) == "" {
		return text
	}

	return redactIncidentPII(emailPattern.ReplaceAllString(privacy.RedactText(text), redactedEmailValue))
}

// RedactIdentifier redacts secrets from identifiers while preserving path and
// URL separators where possible.
func RedactIdentifier(identifier string) string {
	if strings.TrimSpace(identifier) == "" {
		return identifier
	}

	return redactIncidentPII(emailPattern.ReplaceAllString(privacy.RedactIdentifier(identifier), redactedEmailValue))
}

// SensitiveValues extracts exact sensitive fragments that should be passed to
// lower-level audit redactors before raw command arguments are persisted.
func SensitiveValues(text string) []string {
	seen := make(map[string]bool)
	values := make([]string, 0)
	values = appendSensitiveMatches(values, seen, emailPattern.FindAllString(text, -1)...)
	values = appendSensitiveMatches(values, seen, ipAddressSensitiveValues(text)...)
	values = appendSensitiveBearerValues(values, seen, text)
	values = appendSensitiveAssignmentValues(values, seen, incidentSensitiveQuotedValues, text)
	values = appendSensitiveAssignmentValues(values, seen, incidentSensitiveValues, text)
	values = appendSensitiveFlagValues(values, seen, text)

	return values
}

func appendSensitiveBearerValues(values []string, seen map[string]bool, text string) []string {
	for _, match := range bearerTokenValuePattern.FindAllStringSubmatch(text, -1) {
		if len(match) < 2 {
			continue
		}

		values = appendSensitiveMatches(values, seen, match[1])
	}

	return values
}

func appendSensitiveAssignmentValues(values []string, seen map[string]bool, pattern *regexp.Regexp, text string) []string {
	for _, match := range pattern.FindAllStringSubmatch(text, -1) {
		if len(match) < 2 {
			continue
		}

		values = appendSensitiveMatches(values, seen, cleanSensitiveValue(match[1]))
	}

	return values
}

func appendSensitiveFlagValues(values []string, seen map[string]bool, text string) []string {
	for _, match := range incidentSensitiveFlagValues.FindAllStringSubmatch(text, -1) {
		if len(match) < 4 {
			continue
		}

		values = appendSensitiveMatches(values, seen, cleanSensitiveValue(match[3]))
	}

	return values
}

func appendSensitiveMatches(values []string, seen map[string]bool, matches ...string) []string {
	for _, value := range matches {
		value = cleanSensitiveValue(value)
		if value == "" || seen[value] {
			continue
		}

		seen[value] = true
		values = append(values, value)
	}

	return values
}

func cleanSensitiveValue(value string) string {
	value = strings.TrimSpace(value)
	value = strings.Trim(value, `"'`)
	switch strings.ToLower(value) {
	case "", "null", jsonTrueValue, "false", redactedIncidentValue, redactedEmailValue, redactedIPValue:
		return ""
	default:
		return value
	}
}

func redactIncidentPII(text string) string {
	text = incidentPIIQuotedAssignments.ReplaceAllString(text, `"$1":"`+redactedIncidentValue+`"`)
	text = incidentPIIAssignments.ReplaceAllString(text, `${1}${2}`+redactedIncidentValue)
	text = incidentSensitiveFlagValues.ReplaceAllString(text, `${1}${2}`+redactedIncidentValue)

	return redactIPAddresses(text)
}

func redactIPAddresses(text string) string {
	text = ipv4Pattern.ReplaceAllString(text, redactedIPValue)

	return ipv6Pattern.ReplaceAllStringFunc(text, func(match string) string {
		if !validIPv6Match(match) {
			return match
		}

		return redactedIPValue
	})
}

func ipAddressSensitiveValues(text string) []string {
	values := append([]string(nil), ipv4Pattern.FindAllString(text, -1)...)
	for _, match := range ipv6Pattern.FindAllString(text, -1) {
		if !validIPv6Match(match) {
			continue
		}

		values = append(values, match)
		if clean := cleanIPv6Match(match); clean != match {
			values = append(values, clean)
		}
	}

	return values
}

func validIPv6Match(match string) bool {
	clean := cleanIPv6Match(match)
	if clean == "" {
		return false
	}

	_, err := netip.ParseAddr(clean)

	return err == nil && strings.Contains(clean, ":")
}

func cleanIPv6Match(match string) string {
	match = strings.Trim(strings.TrimSpace(match), "[]")
	if before, _, ok := strings.Cut(match, "%"); ok {
		match = before
	}

	return match
}

// RedactMetadata redacts sensitive values and PII-bearing metadata keys.
func RedactMetadata(metadata map[string]string) map[string]string {
	if len(metadata) == 0 {
		return nil
	}

	out := make(map[string]string, len(metadata))
	for key, value := range privacy.RedactMetadata(metadata) {
		key = RedactIdentifier(key)
		if strings.TrimSpace(key) == "" || strings.TrimSpace(value) == "" {
			continue
		}

		if isIncidentSensitiveKey(key) {
			out[key] = redactedIncidentValue
			continue
		}

		out[key] = RedactText(value)
	}

	if len(out) == 0 {
		return nil
	}

	return out
}

// RedactRequest redacts a request before it is persisted or rendered.
func RedactRequest(request Request) Request {
	return Request{
		Method:   RedactText(request.Method),
		URL:      RedactIdentifier(request.URL),
		Headers:  RedactMetadata(request.Headers),
		Data:     RedactText(request.Data),
		Metadata: RedactMetadata(request.Metadata),
	}
}

func redactStackFrames(frames []StackFrame) []StackFrame {
	if len(frames) == 0 {
		return nil
	}

	out := make([]StackFrame, 0, len(frames))
	for _, frame := range frames {
		out = append(out, StackFrame{
			File:        RedactIdentifier(frame.File),
			AbsPath:     RedactIdentifier(frame.AbsPath),
			Function:    RedactText(frame.Function),
			Module:      RedactText(frame.Module),
			ContextLine: RedactText(frame.ContextLine),
			Line:        frame.Line,
			Column:      frame.Column,
			InApp:       frame.InApp,
		})
	}

	return out
}

func redactObservations(values []Observation) []Observation {
	if len(values) == 0 {
		return nil
	}

	out := make([]Observation, 0, len(values))
	for _, value := range values {
		out = append(out, Observation{
			Timestamp: value.Timestamp,
			Source:    RedactText(value.Source),
			Name:      RedactText(value.Name),
			Message:   RedactText(value.Message),
			Fields:    RedactMetadata(value.Fields),
		})
	}

	return out
}

func redactDeployments(values []Deployment) []Deployment {
	if len(values) == 0 {
		return nil
	}

	out := make([]Deployment, 0, len(values))
	for _, value := range values {
		out = append(out, Deployment{
			DeployedAt:  value.DeployedAt,
			Version:     RedactIdentifier(value.Version),
			Commit:      RedactIdentifier(value.Commit),
			Environment: RedactText(value.Environment),
		})
	}

	return out
}

func isIncidentSensitiveKey(key string) bool {
	key = normalizeMetadataKey(key)

	if privacy.IsSensitiveKey(key) {
		return true
	}

	switch key {
	case "state",
		"oauth_state",
		"authorization_code",
		"auth_code",
		"oauth_code",
		"code_verifier",
		"session_id",
		"sid",
		"jwt",
		"nonce",
		"saml_response":
		return true
	}

	for _, marker := range []string{
		"cookie",
		"email",
		"ip",
		"ip_address",
		"remote_addr",
		"user",
		"username",
		"user_id",
		"account_id",
	} {
		if key == marker || strings.HasSuffix(key, "_"+marker) || strings.Contains(key, marker+"_") {
			return true
		}
	}

	return false
}

func applyGenericFields(inc *Context, root map[string]any) {
	if inc == nil {
		return
	}

	resourceLabels := resourceLabelsFromAny(root["resource"])
	serviceContext := objectValue(root["serviceContext"])
	inc.Source = firstNonEmpty(inc.Source, stringValue(root, "source", "provider", "kind"))
	inc.Reference = firstNonEmpty(inc.Reference, stringValue(root,
		"reference", "ref", "id",
		"issue_id", "issueId",
		"alert_id", "alertId", "alertname",
		"event_id", "eventId",
		"trace_id", "traceId",
		"span_id", "spanId",
		"log_id", "logId",
		"operation_Id", "operationId", "operation_id",
		"groupId", "group_id",
	))
	inc.URL = firstNonEmpty(inc.URL, stringValue(root, "url", "permalink", "link"))
	inc.Service = firstNonEmpty(inc.Service, stringValue(root, "service", "service_name", "project", "cloud_RoleName", "cloudRoleName", "appName", "app_name"), stringValue(serviceContext, "service"), labelValue(resourceLabels, "service.name", "service_name", "service", "container_name", "pod_name"))
	inc.Environment = firstNonEmpty(inc.Environment, stringValue(root, "environment", "env"), labelValue(resourceLabels, "environment", "env", "namespace_name"))
	inc.ErrorType = firstNonEmpty(inc.ErrorType, stringValue(root, "error_type", "type", "exception", "problemId", "problem_id", "outerType", "outer_type", "innermostType", "innermost_type"))
	inc.Message = firstNonEmpty(inc.Message, stringValue(root, "message", "error", "error_message", "textPayload", "text_payload", "@message", "body", "outerMessage", "outer_message", "innermostMessage", "innermost_message"))
	inc.Title = firstNonEmpty(inc.Title, stringValue(root, "title", "name"))
	inc.Release = firstNonEmpty(inc.Release, stringValue(root, "release"))
	inc.Version = firstNonEmpty(inc.Version, stringValue(root, "version"), stringValue(serviceContext, "version"))
	inc.Commit = firstNonEmpty(inc.Commit, stringValue(root, "commit", "sha", "revision"))
	applyGenericErrorObject(inc, firstAny(root, "error", "exception", "failure", "exceptions", "details"))
	applyGenericAttributeFields(inc, root)
	applyGenericDatadogData(inc, root)
	applyGenericOTLPCollections(inc, root)
	applyGenericAlertmanager(inc, root)
	applyGenericProtoPayload(inc, root)
	applyGenericCloudWatchAlarm(inc, root)
	applyGenericAzureMonitorTables(inc, root)
	applyGenericTimes(inc, root)
	applyGenericTags(inc, root, resourceLabels, serviceContext)
	applyTagServiceHints(inc)
	applyGenericContextCollections(inc, root)
	applyGenericNestedPayloadFields(inc, root)
}

func applyGenericTimes(inc *Context, root map[string]any) {
	if inc.FirstSeen.IsZero() {
		inc.FirstSeen = timeValue(root, "first_seen", "firstSeen")
	}
	if inc.LastSeen.IsZero() {
		inc.LastSeen = timeValue(root, "last_seen", "lastSeen", "lastSeenTime")
	}
	if inc.Timestamp.IsZero() {
		inc.Timestamp = timeValue(root, "timestamp", "time", "created_at", "dateCreated", "eventTime", "event_time")
	}
	if inc.FirstSeen.IsZero() {
		inc.FirstSeen = timeValue(root, "firstSeenTime")
	}
}

func applyGenericTags(inc *Context, root map[string]any, resourceLabels map[string]string, serviceContext map[string]any) {
	if len(inc.Tags) == 0 {
		inc.Tags = tagsFromAny(root["tags"])
	}
	if len(inc.Tags) == 0 {
		inc.Tags = tagsFromAny(root["labels"])
	}
	if len(inc.Tags) == 0 {
		inc.Tags = resourceLabels
	}
	inc.Tags = mergeStringMaps(inc.Tags, serviceContextTags(serviceContext))
}

func applyTagServiceHints(inc *Context) {
	if inc == nil || len(inc.Tags) == 0 {
		return
	}

	inc.Service = firstNonEmpty(inc.Service, tagValue(inc.Tags, "service.name", "service_name", "service", "app", "application", "functionname", "function_name", "pod", "container"))
	inc.Environment = firstNonEmpty(inc.Environment, tagValue(inc.Tags, "deployment.environment.name", "deployment.environment", "environment", "env", "namespace", "kubernetes_namespace"))
	inc.Release = firstNonEmpty(inc.Release, tagValue(inc.Tags, "release", "deployment.version", "deploymentversion", "version"))
	inc.Version = firstNonEmpty(inc.Version, tagValue(inc.Tags, "service.version", "deployment.version", "deploymentversion", "version"))
}

func applyGenericContextCollections(inc *Context, root map[string]any) {
	inc.Request = mergeGenericRequest(inc.Request, requestFromAny(root["request"]))
	inc.Request = mergeGenericRequest(inc.Request, requestFromAny(firstAny(root, "httpRequest", "http_request")))
	inc.Request = mergeGenericRequest(inc.Request, requestFromRootFields(root))
	if incidentContext := objectValue(root["context"]); incidentContext != nil {
		inc.Request = mergeGenericRequest(inc.Request, requestFromAny(firstAny(incidentContext, "httpRequest", "http_request")))
		if len(inc.StackTrace) == 0 {
			inc.StackTrace = stackFramesFromAny(firstAny(incidentContext, "reportLocation", "report_location"))
		}
	}
	if len(inc.StackTrace) == 0 {
		inc.StackTrace = stackFramesFromAny(firstAny(root, "stack_trace", "stacktrace", "frames", "parsedStack", "parsed_stack", "reportLocation", "report_location"))
	}
	if len(inc.StackTrace) == 0 {
		inc.StackTrace = stackFramesFromAny(inc.Message)
	}
	applyGenericLogCollections(inc, root)
	if len(inc.Traces) == 0 {
		inc.Traces = observationsFromAny(firstAny(root, "traces", "trace_spans", "spans"), "trace")
	}
	applyTraceServiceHints(inc)
	applyObservationStackHints(inc)
	if len(inc.Metrics) == 0 {
		inc.Metrics = observationsFromAny(firstAny(root, "metrics", "measurements"), "metric")
	}
	if len(inc.Metrics) == 0 {
		inc.Metrics = prometheusMetricsFromAny(root)
	}
	applyMetricServiceHints(inc)
	if len(inc.Deployments) == 0 {
		inc.Deployments = deploymentsFromAny(firstAny(root, "deployments", "deployment", "releases"))
	}
}

func applyGenericLogCollections(inc *Context, root map[string]any) {
	if len(inc.Logs) == 0 {
		inc.Logs = observationsFromAny(firstAny(root, "logs", "log_events", "logEvents", "events", "records"), "log")
	}
	if len(inc.Logs) == 0 && kubernetesEventList(root) {
		inc.Logs = observationsFromAny(root["items"], "kubernetes")
	}
	if len(inc.Logs) == 0 && kubernetesEventPayload(root) {
		inc.Logs = observationsFromAny(root, "kubernetes")
	}
	if len(inc.Logs) == 0 {
		inc.Logs = lokiObservationsFromAny(root)
	}
	if len(inc.Logs) == 0 {
		inc.Logs = cloudWatchLogsInsightsObservationsFromAny(root)
	}
	applyObservationServiceHints(inc)
}

func applyGenericNestedPayloadFields(inc *Context, root map[string]any) {
	for _, key := range []string{"jsonPayload", "json_payload", "incident", "payload", "issue", "event", "alert"} {
		payload := objectValue(root[key])
		if payload == nil {
			continue
		}

		applyGenericFields(inc, payload)
	}
}

func resourceLabelsFromAny(value any) map[string]string {
	root := objectValue(value)
	if root == nil {
		return nil
	}

	return stringMapFromAny(root["labels"])
}

func serviceContextTags(root map[string]any) map[string]string {
	if root == nil {
		return nil
	}

	return scalarFields(root, "service", "version")
}

func applyGenericErrorObject(inc *Context, value any) {
	if values := arrayValue(value); len(values) > 0 {
		for _, value := range values {
			applyGenericErrorObject(inc, value)
		}

		return
	}

	root := objectValue(value)
	if inc == nil || root == nil {
		return
	}

	inc.ErrorType = firstNonEmpty(inc.ErrorType, stringValue(root, "type", "error_type", "class", "name", "problemId", "problem_id", "outerType", "outer_type", "innermostType", "innermost_type"))
	inc.Message = firstNonEmpty(inc.Message, stringValue(root, "message", "error", "value", "description", "outerMessage", "outer_message", "innermostMessage", "innermost_message"))
	if len(inc.StackTrace) == 0 {
		inc.StackTrace = stackFramesFromAny(firstAny(root, "stack_trace", "stacktrace", "frames", "parsedStack", "parsed_stack"))
	}
}

func applyGenericAttributeFields(inc *Context, root map[string]any) {
	if inc == nil {
		return
	}

	attributes := genericAttributeMap(root)
	if len(attributes) == 0 {
		return
	}

	applyGenericAttributes(inc, attributes)
}

func applyGenericAttributes(inc *Context, attributes map[string]string) {
	if inc == nil || len(attributes) == 0 {
		return
	}

	inc.Service = firstNonEmpty(inc.Service, attributeValue(attributes, "service.name", "service_name", "service", "cloud_rolename", "cloud.rolename", "cloud.role.name", "approlename", "appname", "app.name"))
	inc.Reference = firstNonEmpty(inc.Reference, attributeValue(attributes,
		"incident.reference", "incident.id",
		"issue.id", "issue_id", "issueid",
		"alert.id", "alert_id", "alertid", "alertname",
		"event.id", "event_id", "eventid",
		"trace.id", "trace_id", "traceid",
		"span.id", "span_id", "spanid",
		"log.id", "log_id", "logid",
		"operation.id", "operation_id", "operationid",
		"item.id", "item_id", "itemid",
		"group.id", "group_id", "groupid",
	))
	inc.Environment = firstNonEmpty(inc.Environment, attributeValue(attributes, "deployment.environment.name", "deployment.environment", "environment", "env", "aspnetcoreenvironment"))
	inc.ErrorType = firstNonEmpty(inc.ErrorType, attributeValue(attributes, "exception.type", "error.type", "error_type", "problemid", "outertype", "innermosttype"))
	inc.Message = firstNonEmpty(inc.Message, attributeValue(attributes, "exception.message", "error.message", "error_message", "log.record.body", "message", "outermessage", "innermostmessage"))
	inc.Release = firstNonEmpty(inc.Release, attributeValue(attributes, "release", "deployment.version", "deploymentversion", "appversion"))
	inc.Version = firstNonEmpty(inc.Version, attributeValue(attributes, "service.version", "version", "deploymentversion", "appversion"))
	inc.Commit = firstNonEmpty(inc.Commit, attributeValue(attributes, "vcs.revision", "git.commit.sha", "commit", "sha"))
	if inc.Timestamp.IsZero() {
		inc.Timestamp = parseTimestamp(attributeValue(attributes, "timestamp", "@timestamp", "date", "timegenerated"))
	}
	inc.Request = mergeGenericRequest(inc.Request, requestFromAttributes(attributes))
	if len(inc.StackTrace) == 0 {
		inc.StackTrace = stackFramesFromAny(attributeValue(attributes, "exception.stacktrace", "exception.stack_trace", "error.stack", "error.stack_trace", "stacktrace"))
	}
}

func applyGenericDatadogData(inc *Context, root map[string]any) {
	if inc == nil || root == nil {
		return
	}

	for _, item := range arrayValue(root["data"]) {
		record := objectValue(item)
		attributes := objectValue(record["attributes"])
		if attributes == nil {
			continue
		}

		inc.Source = firstNonEmpty(inc.Source, stringValue(record, "type"))
		inc.Reference = firstNonEmpty(inc.Reference, stringValue(record, "id"))
		applyGenericFields(inc, attributes)

		log := datadogObservationFromRecord(record, attributes)
		if log.Message != "" || len(log.Fields) > 0 {
			inc.Logs = append(inc.Logs, log)
		}
	}
}

func datadogObservationFromRecord(record, attributes map[string]any) Observation {
	fields := mergeStringMaps(genericAttributeMap(attributes), scalarFields(attributes, "attributes"))
	message := firstNonEmpty(
		stringValue(attributes, "message"),
		attributeValue(fields, "message", "error.message", "exception.message"),
	)

	return Observation{
		Timestamp: timeValue(attributes, "timestamp", "date", "@timestamp"),
		Source:    firstNonEmpty(stringValue(record, "type"), stringValue(attributes, "source"), "datadog"),
		Name:      firstNonEmpty(stringValue(record, "id"), attributeValue(fields, "status", "level", "service.name", "service")),
		Message:   message,
		Fields:    fields,
	}
}

func applyGenericOTLPCollections(inc *Context, root map[string]any) {
	if inc == nil || root == nil {
		return
	}

	applyOTLPResourceSpans(inc, firstAny(root, "resourceSpans", "resource_spans"))
	applyOTLPResourceLogs(inc, firstAny(root, "resourceLogs", "resource_logs"))
}

func applyOTLPResourceSpans(inc *Context, value any) {
	for _, resourceSpan := range arrayValue(value) {
		root := objectValue(resourceSpan)
		resourceAttrs := attributeMapFromAny(root["resource"])
		applyGenericAttributes(inc, resourceAttrs)

		for _, scopeSpan := range arrayValue(firstAny(root, "scopeSpans", "scope_spans", "instrumentationLibrarySpans", "instrumentation_library_spans")) {
			scope := objectValue(scopeSpan)
			for _, span := range arrayValue(scope["spans"]) {
				applyOTLPSpan(inc, resourceAttrs, objectValue(span))
			}
		}
	}
}

func applyOTLPSpan(inc *Context, resourceAttrs map[string]string, span map[string]any) {
	if span == nil {
		return
	}

	attributes := mergeStringMaps(resourceAttrs, attributeMapFromAny(span["attributes"]))
	applyGenericAttributes(inc, attributes)
	inc.Reference = firstNonEmpty(inc.Reference, stringValue(span, "traceId", "trace_id", "spanId", "span_id"))

	fields := mergeStringMaps(attributes, scalarFields(span, "attributes", "events"))
	inc.Traces = append(inc.Traces, Observation{
		Timestamp: timeValue(span, "startTimeUnixNano", "start_time_unix_nano", "startTime", "start_time"),
		Source:    "otel",
		Name:      firstNonEmpty(stringValue(span, "name"), attributeValue(attributes, "http.route", "rpc.method")),
		Fields:    fields,
	})

	for _, event := range arrayValue(span["events"]) {
		applyOTLPEvent(inc, attributes, objectValue(event))
	}
}

func applyOTLPEvent(inc *Context, spanAttrs map[string]string, event map[string]any) {
	if event == nil {
		return
	}

	attributes := mergeStringMaps(spanAttrs, attributeMapFromAny(event["attributes"]))
	applyGenericAttributes(inc, attributes)

	message := firstNonEmpty(attributeValue(attributes, "exception.message", "message"), stringValue(event, "name"))
	inc.Logs = append(inc.Logs, Observation{
		Timestamp: timeValue(event, "timeUnixNano", "time_unix_nano", "time"),
		Source:    "otel",
		Name:      stringValue(event, "name"),
		Message:   message,
		Fields:    attributes,
	})
}

func applyOTLPResourceLogs(inc *Context, value any) {
	for _, resourceLog := range arrayValue(value) {
		root := objectValue(resourceLog)
		resourceAttrs := attributeMapFromAny(root["resource"])
		applyGenericAttributes(inc, resourceAttrs)

		for _, scopeLog := range arrayValue(firstAny(root, "scopeLogs", "scope_logs", "instrumentationLibraryLogs", "instrumentation_library_logs")) {
			scope := objectValue(scopeLog)
			for _, record := range arrayValue(firstAny(scope, "logRecords", "log_records")) {
				applyOTLPLogRecord(inc, resourceAttrs, objectValue(record))
			}
		}
	}
}

func applyOTLPLogRecord(inc *Context, resourceAttrs map[string]string, record map[string]any) {
	if record == nil {
		return
	}

	attributes := mergeStringMaps(resourceAttrs, attributeMapFromAny(record["attributes"]))
	applyGenericAttributes(inc, attributes)
	inc.Reference = firstNonEmpty(inc.Reference, stringValue(record, "traceId", "trace_id", "spanId", "span_id"))

	message := firstNonEmpty(
		attributeValue(attributes, "exception.message", "log.record.body", "message"),
		attributeAnyString(record["body"]),
		stringValue(record, "body"),
	)
	fields := mergeStringMaps(attributes, scalarFields(record, "attributes", "body"))
	inc.Logs = append(inc.Logs, Observation{
		Timestamp: timeValue(record, "timeUnixNano", "time_unix_nano", "observedTimeUnixNano", "observed_time_unix_nano", "time"),
		Source:    "otel",
		Name:      stringValue(record, "severityText", "severity_text", "severityNumber", "severity_number"),
		Message:   message,
		Fields:    fields,
	})
}

func applyGenericAlertmanager(inc *Context, root map[string]any) {
	alerts := arrayValue(root["alerts"])
	commonLabels := stringMapFromAny(firstAny(root, "commonLabels", "common_labels"))
	commonAnnotations := stringMapFromAny(firstAny(root, "commonAnnotations", "common_annotations"))
	if inc == nil || root == nil || (len(alerts) == 0 && len(commonLabels) == 0 && len(commonAnnotations) == 0) {
		return
	}

	firstAlert := objectValue(firstArrayItem(alerts))
	firstAlertLabels := stringMapFromAny(firstAlert["labels"])
	firstAlertAnnotations := stringMapFromAny(firstAlert["annotations"])

	inc.Source = firstNonEmpty(inc.Source, "alertmanager")
	inc.Reference = firstNonEmpty(
		inc.Reference,
		stringValue(root, "groupKey", "group_key"),
		stringValue(firstAlert, "fingerprint"),
		commonLabels["alertname"],
		firstAlertLabels["alertname"],
	)
	inc.URL = firstNonEmpty(inc.URL, stringValue(root, "externalURL", "external_url"), stringValue(firstAlert, "generatorURL", "generator_url"))
	inc.Title = firstNonEmpty(inc.Title, commonAnnotations["summary"], firstAlertAnnotations["summary"], commonLabels["alertname"], firstAlertLabels["alertname"])
	inc.Message = firstNonEmpty(
		inc.Message,
		commonAnnotations["description"],
		firstAlertAnnotations["description"],
		commonAnnotations["message"],
		firstAlertAnnotations["message"],
		commonAnnotations["summary"],
		firstAlertAnnotations["summary"],
	)
	inc.Service = firstNonEmpty(inc.Service, alertmanagerLabelValue(commonLabels, firstAlertLabels, "service", "service_name", "service.name", "job", "app", "container", "pod"))
	inc.Environment = firstNonEmpty(inc.Environment, alertmanagerLabelValue(commonLabels, firstAlertLabels, "environment", "env", "namespace", "kubernetes_namespace"))
	if inc.Timestamp.IsZero() {
		inc.Timestamp = timeValue(firstAlert, "startsAt", "starts_at", "updatedAt", "updated_at")
	}
	inc.Tags = mergeStringMaps(inc.Tags, stringMapFromAny(firstAny(root, "groupLabels", "group_labels")))
	inc.Tags = mergeStringMaps(inc.Tags, commonLabels)

	for _, value := range alerts {
		alert := objectValue(value)
		observation := alertmanagerObservation(alert, commonLabels, commonAnnotations)
		if observation.Name == "" && observation.Message == "" && len(observation.Fields) == 0 {
			continue
		}

		inc.Logs = append(inc.Logs, observation)
		if len(inc.Logs) >= 50 {
			break
		}
	}
}

func alertmanagerObservation(alert map[string]any, commonLabels, commonAnnotations map[string]string) Observation {
	labels := stringMapFromAny(alert["labels"])
	annotations := stringMapFromAny(alert["annotations"])
	fields := mergeStringMaps(commonLabels, labels)
	fields = mergeStringMaps(fields, commonAnnotations)
	fields = mergeStringMaps(fields, annotations)
	fields = mergeStringMaps(fields, scalarFields(alert, "labels", "annotations"))

	return Observation{
		Timestamp: timeValue(alert, "startsAt", "starts_at", "updatedAt", "updated_at", "endsAt", "ends_at"),
		Source:    "alertmanager",
		Name:      firstNonEmpty(labels["alertname"], commonLabels["alertname"], stringValue(alert, "fingerprint")),
		Message: firstNonEmpty(
			annotations["description"],
			annotations["summary"],
			annotations["message"],
			commonAnnotations["description"],
			commonAnnotations["summary"],
			commonAnnotations["message"],
			stringValue(alert, "status"),
		),
		Fields: fields,
	}
}

func alertmanagerLabelValue(commonLabels, alertLabels map[string]string, keys ...string) string {
	for _, key := range keys {
		if value := strings.TrimSpace(alertLabels[key]); value != "" {
			return value
		}
		if value := strings.TrimSpace(commonLabels[key]); value != "" {
			return value
		}
	}

	return ""
}

func applyGenericProtoPayload(inc *Context, root map[string]any) {
	if inc == nil || root == nil {
		return
	}

	proto := objectValue(firstAny(root, "protoPayload", "proto_payload"))
	if proto == nil {
		return
	}

	status := objectValue(proto["status"])
	requestMetadata := objectValue(firstAny(proto, "requestMetadata", "request_metadata"))
	authInfo := objectValue(firstAny(proto, "authenticationInfo", "authentication_info"))
	fields := protoPayloadMetadata(proto, requestMetadata, authInfo, status)

	inc.Service = firstNonEmpty(inc.Service, stringValue(proto, "serviceName", "service_name", "service"))
	inc.ErrorType = firstNonEmpty(inc.ErrorType, stringValue(status, "code"))
	inc.Message = firstNonEmpty(
		inc.Message,
		stringValue(status, "message"),
		stringValue(proto, "methodName", "method_name", "resourceName", "resource_name"),
	)
	inc.Request.Metadata = mergeStringMaps(inc.Request.Metadata, fields)

	observation := Observation{
		Timestamp: timeValue(root, "timestamp", "time", "receiveTimestamp", "receive_timestamp"),
		Source:    firstNonEmpty(stringValue(root, "source"), "gcp-audit"),
		Name:      firstNonEmpty(stringValue(proto, "methodName", "method_name"), stringValue(proto, "serviceName", "service_name")),
		Message:   firstNonEmpty(stringValue(status, "message"), stringValue(proto, "resourceName", "resource_name")),
		Fields:    fields,
	}
	if observation.Name != "" || observation.Message != "" || len(observation.Fields) > 0 {
		inc.Logs = append(inc.Logs, observation)
	}
}

func protoPayloadMetadata(proto, requestMetadata, authInfo, status map[string]any) map[string]string {
	fields := scalarFields(proto, "status", "requestMetadata", "request_metadata", "authenticationInfo", "authentication_info")
	fields = mergeStringMaps(fields, stringMapFromAny(requestMetadata))
	fields = mergeStringMaps(fields, stringMapFromAny(authInfo))
	fields = mergeStringMaps(fields, scalarFields(status))

	return fields
}

func applyGenericCloudWatchAlarm(inc *Context, root map[string]any) {
	if inc == nil || root == nil || !cloudWatchAlarmPayload(root) {
		return
	}

	detail := objectValue(root["detail"])
	state := objectValue(detail["state"])
	configuration := objectValue(detail["configuration"])
	fields := cloudWatchAlarmFields(root, detail, state, configuration)
	alarmName := stringValue(detail, "alarmName", "alarm_name", "name")
	reason := stringValue(state, "reason", "message", "reasonData", "reason_data")

	inc.Source = firstNonEmpty(inc.Source, stringValue(root, "source"), "cloudwatch")
	inc.Reference = firstNonEmpty(
		stringValue(detail, "alarmArn", "alarm_arn", "alarmName", "alarm_name"),
		inc.Reference,
		stringValue(root, "id"),
	)
	inc.Title = firstNonEmpty(inc.Title, alarmName, stringValue(root, "detail-type", "detail_type"))
	inc.ErrorType = firstNonEmpty(inc.ErrorType, stringValue(state, "value", "status"))
	inc.Message = firstNonEmpty(inc.Message, reason)
	if inc.Timestamp.IsZero() {
		inc.Timestamp = firstNonZeroTime(
			timeValue(state, "timestamp", "time"),
			timeValue(root, "time", "timestamp"),
		)
	}
	inc.Service = firstNonEmpty(inc.Service, cloudWatchAlarmService(fields))
	inc.Environment = firstNonEmpty(inc.Environment, cloudWatchAlarmEnvironment(fields))

	observation := Observation{
		Timestamp: inc.Timestamp,
		Source:    "cloudwatch",
		Name:      firstNonEmpty(alarmName, stringValue(root, "detail-type", "detail_type")),
		Message:   reason,
		Fields:    fields,
	}
	if observation.Name != "" || observation.Message != "" || len(observation.Fields) > 0 {
		inc.Metrics = append(inc.Metrics, observation)
	}
}

func cloudWatchAlarmPayload(root map[string]any) bool {
	detail := objectValue(root["detail"])
	if detail == nil {
		return false
	}

	source := strings.ToLower(stringValue(root, "source"))
	detailType := strings.ToLower(stringValue(root, "detail-type", "detail_type"))

	return strings.Contains(source, "cloudwatch") ||
		strings.Contains(detailType, "cloudwatch alarm") ||
		stringValue(detail, "alarmName", "alarm_name", "alarmArn", "alarm_arn") != ""
}

func cloudWatchAlarmFields(root, detail, state, configuration map[string]any) map[string]string {
	fields := scalarFields(root, "detail")
	fields = mergeStringMaps(fields, scalarFields(detail, "state", "configuration", "metrics"))
	fields = mergeStringMaps(fields, prefixedStringMap("state.", scalarFields(state)))

	for _, metric := range arrayValue(firstAny(configuration, "metrics", "Metrics")) {
		fields = mergeStringMaps(fields, cloudWatchAlarmMetricFields(objectValue(metric)))
	}
	fields = mergeStringMaps(fields, cloudWatchDimensionFields(firstAny(detail, "dimensions", "Dimensions")))
	fields = mergeStringMaps(fields, tagsFromAny(firstAny(detail, "tags", "Tags")))

	return fields
}

func cloudWatchAlarmMetricFields(root map[string]any) map[string]string {
	if root == nil {
		return nil
	}

	fields := scalarFields(root, "metricStat", "metric_stat", "expression")
	metricStat := objectValue(firstAny(root, "metricStat", "metric_stat"))
	if metricStat != nil {
		fields = mergeStringMaps(fields, prefixedStringMap("metric_stat.", scalarFields(metricStat, "metric")))
	}

	metric := objectValue(firstAny(metricStat, "metric", "Metric"))
	if metric == nil {
		metric = objectValue(firstAny(root, "metric", "Metric"))
	}
	if metric != nil {
		fields = mergeStringMaps(fields, prefixedStringMap("metric.", scalarFields(metric, "dimensions", "Dimensions")))
		fields = mergeStringMaps(fields, cloudWatchDimensionFields(firstAny(metric, "dimensions", "Dimensions")))
	}

	return fields
}

func cloudWatchDimensionFields(value any) map[string]string {
	dimensions := tagsFromAny(value)
	if len(dimensions) == 0 {
		return nil
	}

	fields := make(map[string]string, len(dimensions)*2)
	for key, value := range dimensions {
		fields[key] = value
		fields["dimension."+key] = value
	}

	return fields
}

func prefixedStringMap(prefix string, values map[string]string) map[string]string {
	if len(values) == 0 {
		return nil
	}

	out := make(map[string]string, len(values))
	for key, value := range values {
		if strings.TrimSpace(key) != "" && strings.TrimSpace(value) != "" {
			out[prefix+key] = value
		}
	}

	return out
}

func cloudWatchAlarmService(fields map[string]string) string {
	return firstNonEmpty(
		fields["ServiceName"],
		fields["service"],
		fields["service_name"],
		fields["service.name"],
		fields["FunctionName"],
		fields["dimension.FunctionName"],
		fields["TargetGroup"],
		fields["LoadBalancer"],
		fields["ClusterName"],
		fields["PodName"],
		fields["InstanceId"],
	)
}

func cloudWatchAlarmEnvironment(fields map[string]string) string {
	return firstNonEmpty(
		fields["Environment"],
		fields["environment"],
		fields["env"],
		fields["dimension.Environment"],
		fields["dimension.environment"],
		fields["Namespace"],
		fields["dimension.Namespace"],
	)
}

func applyGenericAzureMonitorTables(inc *Context, root map[string]any) {
	if inc == nil || root == nil {
		return
	}

	for _, value := range arrayValue(root["tables"]) {
		table := objectValue(value)
		tableName := stringValue(table, "name")
		columns := azureMonitorTableColumns(table["columns"])
		if len(columns) == 0 {
			continue
		}

		for _, rowValue := range arrayValue(table["rows"]) {
			row := azureMonitorTableRow(columns, arrayValue(rowValue))
			if len(row) == 0 {
				continue
			}

			applyAzureMonitorTableRowFields(inc, row)
			observation := azureMonitorTableObservation(tableName, row)
			if observation.Message == "" && observation.Name == "" && len(observation.Fields) == 0 {
				continue
			}

			switch azureMonitorObservationKind(tableName) {
			case "metric":
				inc.Metrics = append(inc.Metrics, observation)
			case "trace":
				inc.Traces = append(inc.Traces, observation)
			default:
				inc.Logs = append(inc.Logs, observation)
			}
			if len(inc.Logs)+len(inc.Traces)+len(inc.Metrics) >= 50 {
				return
			}
		}
	}
}

func azureMonitorTableColumns(value any) []string {
	columns := make([]string, 0)
	for _, item := range arrayValue(value) {
		column := firstNonEmpty(anyString(item), stringValue(objectValue(item), "name", "column", "field"))
		if column == "" {
			continue
		}

		columns = append(columns, column)
	}

	return columns
}

func azureMonitorTableRow(columns []string, values []any) map[string]any {
	if len(columns) == 0 || len(values) == 0 {
		return nil
	}

	row := make(map[string]any, min(len(columns), len(values)))
	for i, column := range columns {
		if i >= len(values) {
			break
		}
		column = strings.TrimSpace(column)
		if column == "" {
			continue
		}

		row[column] = decodeEmbeddedJSONValue(values[i])
	}
	if len(row) == 0 {
		return nil
	}

	return row
}

func decodeEmbeddedJSONValue(value any) any {
	text := strings.TrimSpace(anyString(value))
	if text == "" || (!strings.HasPrefix(text, "{") && !strings.HasPrefix(text, "[")) {
		return value
	}

	var decoded any
	decoder := json.NewDecoder(strings.NewReader(text))
	decoder.UseNumber()
	if err := decoder.Decode(&decoded); err != nil {
		return value
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return value
	}

	return decoded
}

func applyAzureMonitorTableRowFields(inc *Context, row map[string]any) {
	if inc == nil || row == nil {
		return
	}

	applyGenericAttributes(inc, genericAttributeMap(row))
	inc.Reference = firstNonEmpty(inc.Reference, stringValueCase(row,
		"operation_Id", "operationId", "operation_id", "OperationId",
		"itemId", "item_id", "ItemId",
		"id", "Id",
	))
	inc.Service = firstNonEmpty(inc.Service, stringValueCase(row,
		"cloud_RoleName", "cloudRoleName", "cloud_role_name",
		"AppRoleName", "appRoleName", "app_name", "appName",
	))
	inc.Environment = firstNonEmpty(inc.Environment, stringValueCase(row,
		"environment", "env", "Environment",
		"AspNetCoreEnvironment", "aspnetcore_environment",
	))
	inc.ErrorType = firstNonEmpty(inc.ErrorType, stringValueCase(row,
		"problemId", "problem_id", "ProblemId",
		"type", "Type",
		"outerType", "outer_type", "OuterType",
		"innermostType", "innermost_type", "InnermostType",
	))
	inc.Message = firstNonEmpty(inc.Message, stringValueCase(row,
		"message", "Message",
		"outerMessage", "outer_message", "OuterMessage",
		"innermostMessage", "innermost_message", "InnermostMessage",
	))
	inc.Release = firstNonEmpty(inc.Release, stringValueCase(row, "release", "Release", "deploymentVersion", "DeploymentVersion", "appVersion", "AppVersion"))
	inc.Version = firstNonEmpty(inc.Version, stringValueCase(row, "version", "Version", "deploymentVersion", "DeploymentVersion", "appVersion", "AppVersion"))
	if inc.Timestamp.IsZero() {
		inc.Timestamp = timeValueCase(row, "TimeGenerated", "timeGenerated", "timestamp", "Timestamp", "time", "Time")
	}
	applyGenericErrorObject(inc, firstAnyCase(row, "error", "exception", "failure", "exceptions", "details", "Details"))
	if len(inc.StackTrace) == 0 {
		inc.StackTrace = stackFramesFromAny(firstAnyCase(row,
			"stack_trace", "stacktrace", "StackTrace", "Stacktrace",
			"parsedStack", "ParsedStack",
			"stack", "Stack",
		))
	}
}

func azureMonitorTableObservation(tableName string, row map[string]any) Observation {
	fields := azureMonitorObservationFields(row)

	return Observation{
		Timestamp: timeValueCase(row, "TimeGenerated", "timeGenerated", "timestamp", "Timestamp", "time", "Time"),
		Source:    "azure-monitor",
		Name:      firstNonEmpty(tableName, stringValueCase(row, "category", "Category", "severityLevel", "SeverityLevel")),
		Message: firstNonEmpty(
			stringValueCase(row, "message", "Message", "outerMessage", "OuterMessage", "innermostMessage", "InnermostMessage"),
			attributeValue(fields, "message", "outermessage", "innermostmessage"),
		),
		Fields: fields,
	}
}

func azureMonitorObservationFields(row map[string]any) map[string]string {
	fields := make(map[string]string)
	for key, value := range row {
		if text := attributeAnyString(value); strings.TrimSpace(key) != "" && text != "" {
			fields[key] = text
		}
	}
	fields = mergeStringMaps(fields, genericAttributeMap(row))
	if len(fields) == 0 {
		return nil
	}

	return fields
}

func azureMonitorObservationKind(tableName string) string {
	name := strings.ToLower(tableName)
	switch {
	case strings.Contains(name, "metric"):
		return "metric"
	case strings.Contains(name, "request"), strings.Contains(name, "dependency"), strings.Contains(name, "span"), strings.Contains(name, "trace"):
		return "trace"
	default:
		return "log"
	}
}

func genericAttributeMap(root map[string]any) map[string]string {
	out := make(map[string]string)
	for _, value := range []any{
		root["resource"],
		root["resource_attributes"],
		root["resourceAttributes"],
		root["attributes"],
		root["attrs"],
		root["labels"],
		root["customDimensions"],
		root["CustomDimensions"],
		root["custom_dimensions"],
		root["properties"],
		root["Properties"],
	} {
		mergeAttributeMap(out, value)
	}
	if len(out) == 0 {
		return nil
	}

	return out
}

func attributeMapFromAny(value any) map[string]string {
	out := make(map[string]string)
	mergeAttributeMap(out, value)
	if len(out) == 0 {
		return nil
	}

	return out
}

func mergeAttributeMap(out map[string]string, value any) {
	if values := arrayValue(value); len(values) > 0 {
		for _, value := range values {
			mergeAttributeMap(out, value)
		}

		return
	}

	values := objectValue(value)
	if len(values) == 0 {
		return
	}

	if key := stringValue(values, "key", "name"); key != "" {
		if text := attributeAnyString(firstAny(values, "value", "val")); text != "" {
			out[strings.ToLower(key)] = text
		}

		return
	}

	if nested := firstAny(values, "attributes", "resourceAttributes", "resource_attributes"); nested != nil {
		mergeAttributeMap(out, nested)
	}
	for key, value := range values {
		key = strings.ToLower(strings.TrimSpace(key))
		if key == "" || key == "attributes" || key == "resourceattributes" || key == "resource_attributes" {
			continue
		}
		if text := attributeAnyString(value); text != "" {
			out[key] = text
			continue
		}
		mergeNestedAttributeMap(out, value, key)
	}
}

func mergeNestedAttributeMap(out map[string]string, value any, prefix string) {
	if values := arrayValue(value); len(values) > 0 {
		for _, value := range values {
			mergeNestedAttributeMap(out, value, prefix)
		}

		return
	}

	values := objectValue(value)
	if len(values) == 0 {
		return
	}

	for key, value := range values {
		key = strings.ToLower(strings.TrimSpace(key))
		if key == "" {
			continue
		}

		fullKey := prefix + "." + key
		if text := attributeAnyString(value); text != "" {
			out[fullKey] = text
			continue
		}
		mergeNestedAttributeMap(out, value, fullKey)
	}
}

func attributeAnyString(value any) string {
	if text := anyString(value); text != "" {
		return text
	}

	root := objectValue(value)
	if root == nil {
		return ""
	}

	if nested := firstAny(root, "value"); nested != nil {
		if text := attributeAnyString(nested); text != "" {
			return text
		}
	}
	for _, key := range []string{
		"stringValue", "string_value",
		"intValue", "int_value", "integerValue", "integer_value",
		"doubleValue", "double_value", "floatValue", "float_value",
		"boolValue", "bool_value",
	} {
		if text := anyString(root[key]); text != "" {
			return text
		}
	}

	return ""
}

func labelValue(labels map[string]string, keys ...string) string {
	for _, key := range keys {
		if value := strings.TrimSpace(labels[key]); value != "" {
			return value
		}
	}

	return ""
}

func attributeValue(attributes map[string]string, keys ...string) string {
	for _, key := range keys {
		if value := strings.TrimSpace(attributes[strings.ToLower(key)]); value != "" {
			return value
		}
	}

	return ""
}

func requestFromAttributes(attributes map[string]string) Request {
	metadata := make(map[string]string)
	for _, key := range []string{"http.route", "url.path", "http.response.status_code", "http.status_code", "rpc.method", "rpc.service"} {
		if value := attributeValue(attributes, key); value != "" {
			metadata[key] = value
		}
	}

	return Request{
		Method: firstNonEmpty(
			attributeValue(attributes, "http.request.method"),
			attributeValue(attributes, "http.method"),
		),
		URL: firstNonEmpty(
			attributeValue(attributes, "url.full"),
			attributeValue(attributes, "http.url"),
			attributeValue(attributes, "url.path"),
			attributeValue(attributes, "http.target"),
		),
		Metadata: metadata,
	}
}

func requestFromRootFields(root map[string]any) Request {
	if root == nil {
		return Request{}
	}

	metadata := make(map[string]string)
	for _, key := range []string{"resultCode", "result_code", "responseStatusCode", "response_status_code", "status", "success", "duration", "operation_Name", "operationName", "operation_name"} {
		if value := anyString(root[key]); value != "" {
			metadata[key] = value
		}
	}

	method := stringValue(root, "method", "httpMethod", "http_method", "requestMethod", "request_method")
	url := stringValue(root, "requestUrl", "request_url", "url", "uri")
	if method == "" && len(metadata) == 0 {
		url = ""
	}
	if method == "" && url == "" && len(metadata) == 0 {
		return Request{}
	}

	return Request{
		Method:   method,
		URL:      url,
		Metadata: metadata,
	}
}

func mergeGenericRequest(left, right Request) Request {
	return Request{
		Method:   firstNonEmpty(left.Method, right.Method),
		URL:      firstNonEmpty(left.URL, right.URL),
		Data:     firstNonEmpty(left.Data, right.Data),
		Headers:  mergeStringMaps(left.Headers, right.Headers),
		Metadata: mergeStringMaps(left.Metadata, right.Metadata),
	}
}

func requestFromAny(value any) Request {
	root := objectValue(value)
	if root == nil {
		return Request{}
	}

	request := Request{
		Method:   stringValue(root, "method", "http_method", "requestMethod", "request_method"),
		URL:      stringValue(root, "url", "uri", "path", "requestUrl", "request_url"),
		Headers:  stringMapFromAny(root["headers"]),
		Metadata: stringMapFromAny(firstAny(root, "metadata", "context", "query")),
	}
	request.Metadata = mergeStringMaps(request.Metadata, scalarFields(root, "method", "http_method", "requestMethod", "request_method", "url", "uri", "path", "requestUrl", "request_url", "headers", "metadata", "context", "query", "data", "body", "payload"))
	request.Data = firstNonEmpty(
		compactJSONOrString(root["data"]),
		compactJSONOrString(root["body"]),
		compactJSONOrString(root["payload"]),
	)

	return request
}

func stackFramesFromAny(value any) []StackFrame {
	if text := anyString(value); text != "" {
		return stackFramesFromText(text)
	}

	if root := objectValue(value); root != nil {
		if frames := stackFramesFromAny(root["frames"]); len(frames) > 0 {
			return frames
		}

		frame := stackFrameFromMap(root)
		if frame.File != "" || frame.AbsPath != "" || frame.Function != "" {
			return []StackFrame{frame}
		}
	}

	var frames []StackFrame
	for _, value := range arrayValue(value) {
		frame := stackFrameFromMap(objectValue(value))
		if frame.File == "" && frame.AbsPath == "" && frame.Function == "" {
			continue
		}

		frames = append(frames, frame)
	}

	return frames
}

func stackFramesFromText(text string) []StackFrame {
	var frames []StackFrame
	pendingGoFunction := ""
	for line := range strings.Lines(text) {
		if function := goStackFunctionName(line); function != "" {
			pendingGoFunction = function
			continue
		}
		if frame, ok := stackFrameFromTextLine(line); ok {
			if frame.Function == "" && pendingGoFunction != "" && strings.HasSuffix(frame.File, ".go") {
				frame.Function = pendingGoFunction
			}
			frames = append(frames, frame)
			pendingGoFunction = ""
		} else if strings.TrimSpace(line) != "" {
			pendingGoFunction = ""
		}
		if len(frames) >= 50 {
			break
		}
	}

	return frames
}

func goStackFunctionName(line string) string {
	line = strings.TrimSpace(line)
	if line == "" ||
		strings.HasPrefix(line, "goroutine ") ||
		strings.HasPrefix(line, "created by ") ||
		strings.Contains(line, ".go:") ||
		!strings.Contains(line, "(") {
		return ""
	}

	idx := strings.LastIndexByte(line, '(')
	if idx <= 0 {
		return ""
	}

	name := strings.TrimSpace(line[:idx])
	if name == "" || strings.ContainsAny(name, " \t") || !strings.Contains(name, ".") {
		return ""
	}

	return name
}

func stackFramesFromObservations(observations []Observation) []StackFrame {
	var frames []StackFrame
	for i := range observations {
		frames = append(frames, stackFramesFromAny(observations[i].Message)...)
		if len(frames) >= 50 {
			return frames[:50]
		}
		for _, key := range []string{"stacktrace", "stack_trace", "exception.stacktrace", "exception.stack_trace", "error.stack", "error.stacktrace", "error.stack_trace"} {
			frames = append(frames, stackFramesFromAny(observations[i].Fields[key])...)
			if len(frames) >= 50 {
				return frames[:50]
			}
		}
	}

	return frames
}

func kubernetesEventList(root map[string]any) bool {
	if root == nil || len(arrayValue(root["items"])) == 0 {
		return false
	}

	kind := strings.ToLower(stringValue(root, "kind"))
	apiVersion := strings.ToLower(stringValue(root, "apiVersion", "api_version"))

	return strings.Contains(kind, "event") || strings.Contains(apiVersion, "events.k8s.io") || strings.Contains(apiVersion, "k8s")
}

func kubernetesEventPayload(root map[string]any) bool {
	if root == nil {
		return false
	}

	kind := strings.ToLower(stringValue(root, "kind"))
	apiVersion := strings.ToLower(stringValue(root, "apiVersion", "api_version"))
	if !strings.Contains(kind, "event") && !strings.Contains(apiVersion, "events.k8s.io") {
		return false
	}

	return objectValue(root["involvedObject"]) != nil ||
		objectValue(root["regarding"]) != nil ||
		stringValue(root, "reason", "message", "note") != ""
}

func lokiObservationsFromAny(root map[string]any) []Observation {
	if root == nil {
		return nil
	}

	for _, value := range []any{
		objectValue(root["data"])["result"],
		root["result"],
		root["streams"],
	} {
		if observations := lokiObservationsFromStreams(value); len(observations) > 0 {
			return observations
		}
	}

	return nil
}

func lokiObservationsFromStreams(value any) []Observation {
	var observations []Observation
	for _, stream := range arrayValue(value) {
		observations = append(observations, lokiObservationsFromStream(objectValue(stream))...)
		if len(observations) >= 50 {
			return observations[:50]
		}
	}
	if len(observations) > 0 {
		return observations
	}

	return lokiObservationsFromStream(objectValue(value))
}

func lokiObservationsFromStream(root map[string]any) []Observation {
	labels := stringMapFromAny(root["stream"])
	values := arrayValue(root["values"])
	if len(labels) == 0 || len(values) == 0 {
		return nil
	}

	observations := make([]Observation, 0, len(values))
	for _, value := range values {
		pair := arrayValue(value)
		if len(pair) < 2 {
			continue
		}

		message := anyString(pair[1])
		if message == "" {
			continue
		}

		observations = append(observations, Observation{
			Timestamp: timestampFromAny(pair[0]),
			Source:    "loki",
			Name:      firstNonEmpty(labels["pod"], labels["container"], labels["app"], labels["service"], labels["job"]),
			Message:   message,
			Fields:    copyStringMap(labels),
		})
		if len(observations) >= 50 {
			break
		}
	}

	return observations
}

func cloudWatchLogsInsightsObservationsFromAny(root map[string]any) []Observation {
	if root == nil {
		return nil
	}

	if data := objectValue(root["data"]); data != nil {
		if observations := cloudWatchLogsInsightsObservationsFromRows(data["results"]); len(observations) > 0 {
			return observations
		}
	}

	return cloudWatchLogsInsightsObservationsFromRows(root["results"])
}

func cloudWatchLogsInsightsObservationsFromRows(value any) []Observation {
	var observations []Observation
	for _, row := range arrayValue(value) {
		observation := cloudWatchLogsInsightsObservationFromRow(arrayValue(row))
		if observation.Message == "" && len(observation.Fields) == 0 {
			continue
		}

		observations = append(observations, observation)
		if len(observations) >= 50 {
			return observations
		}
	}

	return observations
}

func cloudWatchLogsInsightsObservationFromRow(row []any) Observation {
	fields := make(map[string]string)
	for _, value := range row {
		root := objectValue(value)
		field := stringValue(root, "field", "name")
		if field == "" {
			continue
		}
		if text := stringValue(root, "value"); text != "" {
			fields[field] = text
		}
	}
	if len(fields) == 0 {
		return Observation{}
	}

	return Observation{
		Timestamp: parseTimestamp(firstNonEmpty(fields["@timestamp"], fields["timestamp"], fields["time"])),
		Source:    firstNonEmpty(fields["@log"], fields["logGroup"], fields["log_group"], "cloudwatch"),
		Name:      firstNonEmpty(fields["@logStream"], fields["logStream"], fields["log_stream"], fields["stream"]),
		Message:   firstNonEmpty(fields["@message"], fields["message"], fields["msg"]),
		Fields:    fields,
	}
}

func prometheusMetricsFromAny(root map[string]any) []Observation {
	if root == nil {
		return nil
	}

	if data := objectValue(root["data"]); data != nil {
		if observations := prometheusMetricsFromResult(data["result"]); len(observations) > 0 {
			return observations
		}
	}

	return prometheusMetricsFromResult(root["result"])
}

func prometheusMetricsFromResult(value any) []Observation {
	var observations []Observation
	for _, metric := range arrayValue(value) {
		observations = append(observations, prometheusMetricsFromMap(objectValue(metric))...)
		if len(observations) >= 50 {
			return observations[:50]
		}
	}
	if len(observations) > 0 {
		return observations
	}

	return prometheusMetricsFromMap(objectValue(value))
}

func prometheusMetricsFromMap(root map[string]any) []Observation {
	if root == nil {
		return nil
	}

	labels := stringMapFromAny(root["metric"])
	var observations []Observation
	if pair := arrayValue(root["value"]); len(pair) >= 2 {
		observations = append(observations, prometheusMetricObservation(labels, pair[0], pair[1]))
	}
	for _, value := range arrayValue(root["values"]) {
		pair := arrayValue(value)
		if len(pair) < 2 {
			continue
		}

		observations = append(observations, prometheusMetricObservation(labels, pair[0], pair[1]))
		if len(observations) >= 50 {
			return observations[:50]
		}
	}

	return observations
}

func prometheusMetricObservation(labels map[string]string, timestamp, value any) Observation {
	fields := copyStringMap(labels)
	metricValue := anyString(value)
	if metricValue != "" {
		if fields == nil {
			fields = make(map[string]string)
		}
		fields["value"] = metricValue
	}

	return Observation{
		Timestamp: timestampFromAny(timestamp),
		Source:    "prometheus",
		Name:      firstNonEmpty(labels["__name__"], labels["name"], labels["job"], labels["service"], labels["service.name"]),
		Message:   metricValue,
		Fields:    fields,
	}
}

func applyObservationServiceHints(inc *Context) {
	if inc == nil || len(inc.Logs) == 0 {
		return
	}

	if inc.Service == "" {
		inc.Service = firstObservationField(inc.Logs, "service.name", "service", "service_name", "job", "app", "container", "pod", "involvedObject.name", "regarding.name")
	}
	if inc.Environment == "" {
		inc.Environment = firstObservationField(inc.Logs, "environment", "env", "namespace", "involvedObject.namespace", "regarding.namespace", "metadata.namespace")
	}
}

func applyMetricServiceHints(inc *Context) {
	if inc == nil || len(inc.Metrics) == 0 {
		return
	}

	if inc.Service == "" {
		inc.Service = firstObservationField(inc.Metrics, "service.name", "service", "service_name", "job", "app")
	}
	if inc.Environment == "" {
		inc.Environment = firstObservationField(inc.Metrics, "environment", "env", "namespace", "kubernetes_namespace")
	}
}

func applyTraceServiceHints(inc *Context) {
	if inc == nil || len(inc.Traces) == 0 {
		return
	}

	if inc.Service == "" {
		inc.Service = firstObservationField(inc.Traces, "service.name", "service", "service_name", "resource.service.name", "job", "app")
	}
	if inc.Environment == "" {
		inc.Environment = firstObservationField(inc.Traces, "deployment.environment.name", "deployment.environment", "environment", "env", "namespace")
	}
}

func applyObservationStackHints(inc *Context) {
	if inc == nil || len(inc.StackTrace) > 0 {
		return
	}

	if frames := stackFramesFromObservations(inc.Logs); len(frames) > 0 {
		inc.StackTrace = frames
		return
	}

	inc.StackTrace = stackFramesFromObservations(inc.Traces)
}

func firstObservationField(observations []Observation, keys ...string) string {
	for i := range observations {
		for _, key := range keys {
			if value := strings.TrimSpace(observations[i].Fields[key]); value != "" {
				return value
			}
		}
	}

	return ""
}

func stackFrameFromTextLine(line string) (StackFrame, bool) {
	if match := dotnetStackFramePattern.FindStringSubmatch(line); len(match) > 0 {
		return StackFrame{
			File:     match[2],
			Function: strings.TrimSpace(match[1]),
			Line:     atoiDefault(match[3]),
			InApp:    true,
		}, true
	}
	if match := javaStackFramePattern.FindStringSubmatch(line); len(match) > 0 {
		function := strings.TrimSpace(match[1])
		file := match[2]

		return StackFrame{
			File:     file,
			AbsPath:  inferredJVMFramePath(file, function),
			Function: function,
			Line:     atoiDefault(match[3]),
			InApp:    true,
		}, true
	}
	if match := jsStackFramePattern.FindStringSubmatch(line); len(match) > 0 {
		function := strings.TrimSpace(match[1])
		file := match[2]

		return StackFrame{
			File:     file,
			AbsPath:  inferredJVMFramePath(file, function),
			Function: function,
			Line:     atoiDefault(match[3]),
			Column:   atoiDefault(match[4]),
			InApp:    true,
		}, true
	}
	if match := jsAnonymousFramePattern.FindStringSubmatch(line); len(match) > 0 {
		return StackFrame{
			File:   match[1],
			Line:   atoiDefault(match[2]),
			Column: atoiDefault(match[3]),
			InApp:  true,
		}, true
	}
	if match := pythonStackFramePattern.FindStringSubmatch(line); len(match) > 0 {
		return StackFrame{
			File:     match[1],
			Function: strings.TrimSpace(match[3]),
			Line:     atoiDefault(match[2]),
			InApp:    true,
		}, true
	}
	if match := plainStackFramePattern.FindStringSubmatch(line); len(match) > 0 {
		return StackFrame{
			File:   match[1],
			Line:   atoiDefault(match[2]),
			Column: atoiDefault(match[3]),
			InApp:  true,
		}, true
	}

	return StackFrame{}, false
}

func inferredJVMFramePath(file, function string) string {
	file = strings.TrimSpace(file)
	function = strings.TrimSpace(function)
	if !strings.HasSuffix(file, ".java") || strings.Contains(file, "/") || function == "" {
		return ""
	}

	className := strings.TrimSuffix(file, ".java")
	parts := strings.Split(function, ".")
	for i, part := range parts {
		if part != className && !strings.HasPrefix(part, className+"$") {
			continue
		}

		pathParts := append([]string(nil), parts[:i]...)
		pathParts = append(pathParts, file)

		return strings.Join(pathParts, "/")
	}

	return ""
}

func stackFrameFromMap(root map[string]any) StackFrame {
	if root == nil {
		return StackFrame{}
	}

	return StackFrame{
		File:        firstNonEmpty(stringValue(root, "file", "filename", "fileName", "file_name", "filePath", "file_path", "path")),
		AbsPath:     stringValue(root, "abs_path", "absPath", "absolute_path"),
		Function:    stringValue(root, "function", "functionName", "function_name", "func", "method"),
		Module:      stringValue(root, "module", "package"),
		ContextLine: stringValue(root, "context_line", "contextLine", "line_text"),
		Line:        intValue(root, "line", "lineno", "line_number", "lineNumber"),
		Column:      intValue(root, "column", "colno", "column_number"),
		InApp:       boolValue(root, "in_app", "inApp", "application"),
	}
}

func observationsFromAny(value any, defaultSource string) []Observation {
	if root := objectValue(value); root != nil {
		return []Observation{observationFromMap(root, defaultSource)}
	}

	values := arrayValue(value)
	if len(values) == 0 {
		if message := anyString(value); message != "" {
			return []Observation{{Source: defaultSource, Message: message}}
		}

		return nil
	}

	observations := make([]Observation, 0, len(values))
	for _, value := range values {
		if root := objectValue(value); root != nil {
			observations = append(observations, observationFromMap(root, defaultSource))
			continue
		}
		if message := anyString(value); message != "" {
			observations = append(observations, Observation{Source: defaultSource, Message: message})
		}
	}

	return observations
}

func observationFromMap(root map[string]any, defaultSource string) Observation {
	fields := stringMapFromAny(root["fields"])
	if len(fields) == 0 {
		fields = scalarFields(root, "source", "provider", "backend", "name", "metric", "span", "trace_id", "id", "message", "msg", "description", "value", "status", "timestamp", "time", "ts")
	}
	fields = mergeStringMaps(fields, nestedObservationFields(root))

	return Observation{
		Timestamp: observationTimestamp(root),
		Source: firstNonEmpty(
			stringValue(root, "source", "provider", "backend", "reportingController", "reporting_component", "component"),
			fields["source.component"],
			defaultSource,
		),
		Name:    firstNonEmpty(stringValue(root, "name", "metric", "span", "trace_id", "id", "level", "category", "reason", "action"), fields["metadata.name"]),
		Message: stringValue(root, "message", "msg", "description", "note", "value", "status", "reason"),
		Fields:  fields,
	}
}

func observationTimestamp(root map[string]any) time.Time {
	if timestamp := timeValue(root, "timestamp", "time", "ts", "eventTime", "event_time", "firstTimestamp", "first_timestamp", "lastTimestamp", "last_timestamp"); !timestamp.IsZero() {
		return timestamp
	}

	return timeValue(objectValue(root["metadata"]), "creationTimestamp", "creation_timestamp")
}

func nestedObservationFields(root map[string]any) map[string]string {
	out := make(map[string]string)
	addPrefixedFields(out, "metadata", objectValue(root["metadata"]), "name", "namespace", "uid", "creationTimestamp")
	addPrefixedFields(out, "involvedObject", objectValue(root["involvedObject"]), "kind", "namespace", "name", "uid", "fieldPath")
	addPrefixedFields(out, "regarding", objectValue(root["regarding"]), "kind", "namespace", "name", "uid", "fieldPath")
	addPrefixedFields(out, "source", objectValue(root["source"]), "component", "host")

	if len(out) == 0 {
		return nil
	}

	return out
}

func addPrefixedFields(out map[string]string, prefix string, root map[string]any, keys ...string) {
	if len(root) == 0 {
		return
	}

	for _, key := range keys {
		if value := anyString(root[key]); value != "" {
			out[prefix+"."+key] = value
		}
	}
}

func deploymentsFromAny(value any) []Deployment {
	if root := objectValue(value); root != nil {
		return []Deployment{deploymentFromMap(root)}
	}

	values := arrayValue(value)
	if len(values) == 0 {
		if version := anyString(value); version != "" {
			return []Deployment{{Version: version}}
		}

		return nil
	}

	deployments := make([]Deployment, 0, len(values))
	for _, value := range values {
		if root := objectValue(value); root != nil {
			deployments = append(deployments, deploymentFromMap(root))
			continue
		}
		if version := anyString(value); version != "" {
			deployments = append(deployments, Deployment{Version: version})
		}
	}

	return deployments
}

func deploymentFromMap(root map[string]any) Deployment {
	if root == nil {
		return Deployment{}
	}

	return Deployment{
		DeployedAt:  timeValue(root, "deployed_at", "deployedAt", "created_at", "createdAt", "updated_at", "updatedAt", "finished_at", "finishedAt", "date", "timestamp", "time"),
		Version:     stringValue(root, "version", "release", "name", "ref", "tag"),
		Commit:      stringValue(root, "commit", "sha", "head_sha", "headSha", "revision"),
		Environment: stringValue(root, "environment", "env"),
	}
}

func decodeJSONObject(raw []byte) (map[string]any, error) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 {
		return nil, errors.New("incident: JSON payload is empty")
	}

	var root map[string]any
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if err := decoder.Decode(&root); err != nil {
		return nil, fmt.Errorf("incident: parse JSON payload: %w", err)
	}

	if root == nil {
		return nil, errors.New("incident: JSON payload must be an object")
	}

	return root, nil
}

func sentryEnvelope(root map[string]any) (issue, event map[string]any, ok bool) {
	issue = objectValue(root["issue"])
	event = objectValue(root["event"])
	if issue != nil && !looksLikeSentryPayload(issue) {
		issue = nil
	}
	if event != nil && !looksLikeSentryPayload(event) {
		event = nil
	}
	if issue != nil || event != nil {
		return issue, event, true
	}

	return nil, nil, false
}

func sentryPayloadParts(root map[string]any) (issue, event map[string]any, ok bool) {
	issue, event, ok = sentryEnvelope(root)
	if ok {
		return issue, event, true
	}

	if !looksLikeSentryPayload(root) {
		return nil, nil, false
	}

	if looksLikeSentryEventPayload(root) {
		return nil, root, true
	}

	return root, nil, true
}

func looksLikeSentryPayload(root map[string]any) bool {
	if root == nil {
		return false
	}

	if hasAnyKey(root, "entries", "culprit", "shortId", "short_id", "eventID", "event_id", "issueShortId") {
		return true
	}

	if _, ok := root["metadata"]; !ok {
		return false
	}

	return hasAnyKey(root, "project", "firstSeen", "first_seen", "lastSeen", "last_seen", "permalink")
}

func looksLikeSentryEventPayload(root map[string]any) bool {
	return hasAnyKey(root, "entries", "eventID", "event_id", "issueShortId", "exception")
}

func hasAnyKey(root map[string]any, keys ...string) bool {
	for _, key := range keys {
		if _, ok := root[key]; ok {
			return true
		}
	}

	return false
}

func mustJSON(value map[string]any) []byte {
	if value == nil {
		return nil
	}

	data, err := json.Marshal(value)
	if err != nil {
		return nil
	}

	return data
}

func objectValue(value any) map[string]any {
	obj, ok := value.(map[string]any)
	if !ok {
		return nil
	}

	return obj
}

func arrayValue(value any) []any {
	values, ok := value.([]any)
	if !ok {
		return nil
	}

	return values
}

func firstArrayItem(values []any) any {
	if len(values) == 0 {
		return nil
	}

	return values[0]
}

func stringValue(root map[string]any, keys ...string) string {
	for _, key := range keys {
		if value, ok := root[key]; ok {
			if text := anyString(value); text != "" {
				return text
			}
		}
	}

	return ""
}

func stringValueCase(root map[string]any, keys ...string) string {
	if text := stringValue(root, keys...); text != "" {
		return text
	}

	return anyString(firstAnyCase(root, keys...))
}

func anyString(value any) string {
	switch typed := value.(type) {
	case string:
		return strings.TrimSpace(typed)
	case json.Number:
		return strings.TrimSpace(typed.String())
	case float64:
		return strings.TrimRight(strings.TrimRight(fmt.Sprintf("%.6f", typed), "0"), ".")
	case bool:
		if typed {
			return "true"
		}

		return "false"
	default:
		return ""
	}
}

func intValue(root map[string]any, keys ...string) int {
	for _, key := range keys {
		value, ok := root[key]
		if !ok {
			continue
		}

		switch typed := value.(type) {
		case int:
			return typed
		case float64:
			return int(typed)
		case json.Number:
			number, err := typed.Int64()
			if err == nil {
				return int(number)
			}
		case string:
			if number := atoiDefault(typed); number > 0 {
				return number
			}
		}
	}

	return 0
}

func atoiDefault(raw string) int {
	number, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil {
		return 0
	}

	return number
}

func boolValue(root map[string]any, keys ...string) bool {
	for _, key := range keys {
		value, ok := root[key]
		if !ok {
			continue
		}

		switch typed := value.(type) {
		case bool:
			return typed
		case string:
			switch strings.ToLower(strings.TrimSpace(typed)) {
			case "1", "true", "yes", "on":
				return true
			}
		}
	}

	return false
}

func timeValue(root map[string]any, keys ...string) time.Time {
	for _, key := range keys {
		value, ok := root[key]
		if !ok {
			continue
		}

		if timestamp := timestampFromAny(value); !timestamp.IsZero() {
			return timestamp
		}
	}

	return time.Time{}
}

func timeValueCase(root map[string]any, keys ...string) time.Time {
	if timestamp := timeValue(root, keys...); !timestamp.IsZero() {
		return timestamp
	}

	return timestampFromAny(firstAnyCase(root, keys...))
}

func timestampFromAny(value any) time.Time {
	switch typed := value.(type) {
	case string:
		return parseTimestamp(typed)
	case json.Number:
		if parsed := parseTimestamp(typed.String()); !parsed.IsZero() {
			return parsed
		}
		if seconds, err := typed.Float64(); err == nil {
			return unixSeconds(seconds)
		}
	case float64:
		return unixSeconds(typed)
	}

	return time.Time{}
}

func parseTimestamp(raw string) time.Time {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return time.Time{}
	}

	for _, layout := range []string{time.RFC3339Nano, time.RFC3339, "2006-01-02T15:04:05", "2006-01-02 15:04:05"} {
		parsed, err := time.Parse(layout, raw)
		if err == nil {
			return parsed.UTC()
		}
	}

	if timestamp, ok := unixTimestampString(raw); ok {
		return timestamp
	}

	return time.Time{}
}

func unixTimestampString(raw string) (time.Time, bool) {
	if raw == "" {
		return time.Time{}, false
	}

	allDigits := true
	for _, r := range raw {
		if r < '0' || r > '9' {
			allDigits = false
			break
		}
	}
	if allDigits {
		value, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || value <= 0 {
			return time.Time{}, false
		}
		switch {
		case value > 1e17:
			return time.Unix(0, value).UTC(), true
		case value > 1e14:
			return time.UnixMicro(value).UTC(), true
		case value > 1e11:
			return time.UnixMilli(value).UTC(), true
		default:
			return time.Unix(value, 0).UTC(), true
		}
	}

	seconds, err := strconv.ParseFloat(raw, 64)
	if err != nil {
		return time.Time{}, false
	}

	return unixSeconds(seconds), true
}

func unixSeconds(seconds float64) time.Time {
	if seconds <= 0 {
		return time.Time{}
	}
	switch {
	case seconds > 1e17:
		return time.Unix(0, int64(seconds)).UTC()
	case seconds > 1e14:
		return time.UnixMicro(int64(seconds)).UTC()
	case seconds > 1e11:
		return time.UnixMilli(int64(seconds)).UTC()
	}

	whole := int64(seconds)
	nanos := int64((seconds - float64(whole)) * float64(time.Second))

	return time.Unix(whole, nanos).UTC()
}

func tagsFromAny(value any) map[string]string {
	switch typed := value.(type) {
	case map[string]any:
		return stringMapFromAny(typed)
	case map[string]string:
		return copyStringMap(typed)
	case []any:
		return tagsFromArray(typed)
	default:
		return nil
	}
}

func firstAny(root map[string]any, keys ...string) any {
	for _, key := range keys {
		if value, ok := root[key]; ok && value != nil {
			return value
		}
	}

	return nil
}

func firstAnyCase(root map[string]any, keys ...string) any {
	if value := firstAny(root, keys...); value != nil {
		return value
	}
	if len(root) == 0 {
		return nil
	}

	for _, key := range keys {
		for actual, value := range root {
			if strings.EqualFold(actual, key) && value != nil {
				return value
			}
		}
	}

	return nil
}

func tagsFromArray(values []any) map[string]string {
	out := make(map[string]string)
	for _, value := range values {
		switch typed := value.(type) {
		case map[string]any:
			key := stringValue(typed, "key", "name", "Name")
			val := stringValue(typed, "value", "Value")
			if key != "" && val != "" {
				out[key] = val
			}
		case []any:
			if len(typed) >= 2 {
				key := anyString(typed[0])
				val := anyString(typed[1])
				if key != "" && val != "" {
					out[key] = val
				}
			}
		case string:
			key, val := splitTagString(typed)
			if key != "" && val != "" {
				out[key] = val
			}
		}
	}

	if len(out) == 0 {
		return nil
	}

	return out
}

func splitTagString(raw string) (key, value string) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", ""
	}

	for _, sep := range []string{":", "="} {
		before, after, ok := strings.Cut(raw, sep)
		if !ok {
			continue
		}
		before = strings.TrimSpace(before)
		after = strings.TrimSpace(after)
		if before != "" && after != "" {
			return before, after
		}
	}

	return "", ""
}

func tagValue(tags map[string]string, keys ...string) string {
	if len(tags) == 0 {
		return ""
	}

	for _, key := range keys {
		normalizedKey := normalizeMetadataKey(key)
		for actual, value := range tags {
			if normalizeMetadataKey(actual) == normalizedKey && strings.TrimSpace(value) != "" {
				return strings.TrimSpace(value)
			}
		}
	}

	return ""
}

func normalizeMetadataKey(key string) string {
	key = strings.ToLower(strings.TrimSpace(key))
	key = strings.ReplaceAll(key, "-", "_")
	key = strings.ReplaceAll(key, ".", "_")
	key = strings.ReplaceAll(key, "/", "_")
	key = strings.ReplaceAll(key, ":", "_")

	return key
}

func stringsFromMapAny(values map[string]any) map[string]string {
	return stringMapFromAny(values)
}

func stringMapFromAny(value any) map[string]string {
	values, ok := value.(map[string]any)
	if !ok || len(values) == 0 {
		return nil
	}

	out := make(map[string]string, len(values))
	for key, value := range values {
		if text := anyString(value); strings.TrimSpace(key) != "" && text != "" {
			out[key] = text
		}
	}

	if len(out) == 0 {
		return nil
	}

	return out
}

func scalarFields(values map[string]any, skipKeys ...string) map[string]string {
	if len(values) == 0 {
		return nil
	}

	skip := make(map[string]bool, len(skipKeys))
	for _, key := range skipKeys {
		skip[strings.ToLower(key)] = true
	}

	out := make(map[string]string)
	for key, value := range values {
		if skip[strings.ToLower(key)] {
			continue
		}
		if text := anyString(value); text != "" {
			out[key] = text
		}
	}

	if len(out) == 0 {
		return nil
	}

	return out
}

func copyStringMap(values map[string]string) map[string]string {
	if len(values) == 0 {
		return nil
	}

	out := make(map[string]string, len(values))
	for key, value := range values {
		if strings.TrimSpace(key) != "" && strings.TrimSpace(value) != "" {
			out[key] = value
		}
	}

	if len(out) == 0 {
		return nil
	}

	return out
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}

	return ""
}

func firstNonZeroTime(values ...time.Time) time.Time {
	for _, value := range values {
		if !value.IsZero() {
			return value
		}
	}

	return time.Time{}
}
