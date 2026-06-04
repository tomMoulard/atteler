//nolint:wsl_v5 // Sentry payload normalization keeps adjacent extraction statements close.
package incident

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// ParseSentryIncident normalizes Sentry issue and issue-event JSON payloads.
// The issue payload should come from the Sentry "Retrieve an Issue" endpoint;
// the optional event payload should come from an issue event endpoint/list item
// with full=true so stack traces are available.
func ParseSentryIncident(issueRaw, eventRaw []byte) (Context, error) {
	var inc Context
	inc.Source = SourceSentry

	if len(issueRaw) > 0 {
		issue, err := decodeJSONObject(issueRaw)
		if err != nil {
			return Context{}, fmt.Errorf("sentry issue: %w", err)
		}

		applySentryIssue(&inc, issue)
	}

	if len(eventRaw) > 0 {
		event, err := decodeJSONObject(eventRaw)
		if err != nil {
			return Context{}, fmt.Errorf("sentry event: %w", err)
		}

		applySentryEvent(&inc, event)
	}

	if inc.Reference == "" {
		return Context{}, errors.New("sentry incident: missing issue id or short id")
	}

	if inc.Message == "" {
		inc.Message = firstNonEmpty(inc.Title, inc.ErrorType)
	}

	return RedactContext(inc), nil
}

func applySentryIssue(inc *Context, issue map[string]any) {
	inc.Reference = firstNonEmpty(inc.Reference, stringValue(issue, "shortId", "short_id", "id"))
	inc.URL = firstNonEmpty(inc.URL, stringValue(issue, "permalink", "url"))
	inc.Title = firstNonEmpty(inc.Title, stringValue(issue, "title", "culprit"))
	inc.FirstSeen = firstTime(inc.FirstSeen, timeValue(issue, "firstSeen", "first_seen"))
	inc.LastSeen = firstTime(inc.LastSeen, timeValue(issue, "lastSeen", "last_seen"))

	if project := objectValue(issue["project"]); project != nil {
		inc.Service = firstNonEmpty(inc.Service, stringValue(project, "slug", "name"))
	}

	if metadata := objectValue(issue["metadata"]); metadata != nil {
		inc.ErrorType = firstNonEmpty(inc.ErrorType, stringValue(metadata, "type", "title"))
		inc.Message = firstNonEmpty(inc.Message, stringValue(metadata, "value", "message", "filename"))
		if inc.StackTrace == nil {
			applySentryMetadataFrame(inc, metadata)
		}
	}

	inc.Tags = mergeStringMaps(inc.Tags, tagsFromAny(issue["tags"]))
	applySentryTags(inc)
	applySentryRelease(inc, issue)
	applySentryIssueEmbeddedEvent(inc, issue)
}

func applySentryIssueEmbeddedEvent(inc *Context, issue map[string]any) {
	for _, key := range []string{"latestEvent", "latest_event", "event"} {
		event := objectValue(issue[key])
		if event == nil {
			continue
		}

		applySentryEvent(inc, event)
		return
	}
}

func applySentryEvent(inc *Context, event map[string]any) {
	inc.Reference = firstNonEmpty(inc.Reference, stringValue(event, "issueShortId", "groupID", "groupId", "group_id", "id", "eventID"))
	inc.Title = firstNonEmpty(inc.Title, stringValue(event, "title", "culprit"))
	inc.Message = firstNonEmpty(inc.Message, stringValue(event, "message"))
	inc.Timestamp = firstTime(inc.Timestamp, timeValue(event, "dateCreated", "timestamp", "received"))

	inc.Tags = mergeStringMaps(inc.Tags, tagsFromAny(event["tags"]))
	applySentryTags(inc)
	applySentryRelease(inc, event)

	if project := objectValue(event["project"]); project != nil {
		inc.Service = firstNonEmpty(inc.Service, stringValue(project, "slug", "name"))
	}

	if contexts := objectValue(event["contexts"]); contexts != nil {
		applySentryContexts(inc, contexts)
	}

	for _, entry := range arrayValue(event["entries"]) {
		applySentryEntry(inc, objectValue(entry))
	}

	if exception := objectValue(event["exception"]); exception != nil {
		applySentryException(inc, exception)
	}
}

func applySentryMetadataFrame(inc *Context, metadata map[string]any) {
	file := stringValue(metadata, "filename")
	function := stringValue(metadata, "function")
	if file == "" && function == "" {
		return
	}

	inc.StackTrace = append(inc.StackTrace, StackFrame{
		File:     file,
		Function: function,
		Line:     intValue(metadata, "lineno", "line"),
		InApp:    true,
	})
}

func applySentryRelease(inc *Context, root map[string]any) {
	switch release := root["release"].(type) {
	case string:
		inc.Release = firstNonEmpty(inc.Release, release)
	case map[string]any:
		inc.Release = firstNonEmpty(inc.Release, stringValue(release, "version", "shortVersion", "name"))
		inc.Commit = firstNonEmpty(inc.Commit, sentryReleaseCommit(release))
		appendSentryDeployment(inc, deploymentFromSentryRelease(release, inc.Environment))
	}

	inc.Version = firstNonEmpty(inc.Version, inc.Release, stringValue(root, "dist"))

	for _, key := range []string{"firstRelease", "first_release", "lastRelease", "last_release"} {
		release := objectValue(root[key])
		if release == nil {
			continue
		}

		inc.Release = firstNonEmpty(inc.Release, stringValue(release, "version", "shortVersion", "name"))
		inc.Commit = firstNonEmpty(inc.Commit, sentryReleaseCommit(release))
		appendSentryDeployment(inc, deploymentFromSentryRelease(release, inc.Environment))
	}

	appendSentryDeployment(inc, Deployment{
		Version:     inc.Release,
		Commit:      inc.Commit,
		Environment: inc.Environment,
	})
}

func applySentryTags(inc *Context) {
	if len(inc.Tags) == 0 {
		return
	}

	inc.Environment = firstNonEmpty(inc.Environment, inc.Tags["environment"], inc.Tags["env"])
	inc.Service = firstNonEmpty(inc.Service, inc.Tags["service"], inc.Tags["service.name"], inc.Tags["server_name"])
	inc.Release = firstNonEmpty(inc.Release, inc.Tags["release"])
	inc.Version = firstNonEmpty(inc.Version, inc.Tags["version"], inc.Release)
	inc.Commit = firstNonEmpty(inc.Commit, inc.Tags["commit"], inc.Tags["sha"])
}

func deploymentFromSentryRelease(release map[string]any, environment string) Deployment {
	return Deployment{
		DeployedAt:  firstTime(timeValue(release, "dateReleased", "date_released"), timeValue(release, "dateCreated", "date_created")),
		Version:     stringValue(release, "version", "shortVersion", "name"),
		Commit:      sentryReleaseCommit(release),
		Environment: environment,
	}
}

func sentryReleaseCommit(release map[string]any) string {
	if release == nil {
		return ""
	}

	if commit := firstNonEmpty(stringValue(release, "commit", "last_commit", "ref")); commit != "" {
		return commit
	}

	if lastCommit := objectValue(release["lastCommit"]); lastCommit != nil {
		return stringValue(lastCommit, "id", "shortId", "short_id", "hash")
	}

	return ""
}

func appendSentryDeployment(inc *Context, deployment Deployment) {
	if deployment.Version == "" && deployment.Commit == "" && deployment.Environment == "" && deployment.DeployedAt.IsZero() {
		return
	}

	for _, existing := range inc.Deployments {
		if existing.Version == deployment.Version &&
			existing.Commit == deployment.Commit &&
			existing.Environment == deployment.Environment &&
			existing.DeployedAt.Equal(deployment.DeployedAt) {
			return
		}
	}

	inc.Deployments = append(inc.Deployments, deployment)
}

func applySentryContexts(inc *Context, contexts map[string]any) {
	if trace := objectValue(contexts["trace"]); trace != nil {
		fields := stringsFromMapAny(trace)
		inc.Traces = append(inc.Traces, Observation{
			Source:  SourceSentry,
			Name:    firstNonEmpty(fields["op"], fields["trace_id"], "trace"),
			Message: firstNonEmpty(fields["status"], fields["description"]),
			Fields:  fields,
		})
	}
}

func applySentryEntry(inc *Context, entry map[string]any) {
	if entry == nil {
		return
	}

	data := objectValue(entry["data"])
	switch strings.ToLower(stringValue(entry, "type")) {
	case "exception":
		applySentryException(inc, data)
	case "request":
		inc.Request = mergeSentryRequest(inc.Request, data)
	case "breadcrumbs":
		applySentryBreadcrumbs(inc, data)
	case "message":
		inc.Message = firstNonEmpty(inc.Message, stringValue(data, "message", "formatted"))
	default:
		if message := stringValue(data, "message", "value"); message != "" {
			inc.Logs = append(inc.Logs, Observation{
				Source:  SourceSentry,
				Name:    stringValue(entry, "type"),
				Message: message,
				Fields:  stringsFromMapAny(data),
			})
		}
	}
}

func applySentryException(inc *Context, data map[string]any) {
	if data == nil {
		return
	}

	for _, value := range arrayValue(data["values"]) {
		exception := objectValue(value)
		if exception == nil {
			continue
		}

		inc.ErrorType = firstNonEmpty(inc.ErrorType, stringValue(exception, "type"))
		inc.Message = firstNonEmpty(inc.Message, stringValue(exception, "value"))
		applySentryStacktrace(inc, objectValue(exception["stacktrace"]))
	}

	applySentryStacktrace(inc, objectValue(data["stacktrace"]))
}

func applySentryStacktrace(inc *Context, stacktrace map[string]any) {
	if stacktrace == nil {
		return
	}

	for _, rawFrame := range arrayValue(stacktrace["frames"]) {
		frame := sentryStackFrame(objectValue(rawFrame))
		if frame.File == "" && frame.AbsPath == "" && frame.Function == "" {
			continue
		}

		inc.StackTrace = append(inc.StackTrace, frame)
	}
}

func sentryStackFrame(frame map[string]any) StackFrame {
	if frame == nil {
		return StackFrame{}
	}

	return StackFrame{
		File:     firstNonEmpty(stringValue(frame, "filename"), stringValue(frame, "file")),
		AbsPath:  stringValue(frame, "absPath", "abs_path"),
		Function: stringValue(frame, "function"),
		Module:   stringValue(frame, "module", "package"),
		Line:     intValue(frame, "lineno", "line", "lineNo"),
		Column:   intValue(frame, "colno", "colNo", "column", "columnNo"),
		InApp:    boolValue(frame, "in_app", "inApp"),
		ContextLine: firstNonEmpty(
			stringValue(frame, "context_line", "contextLine"),
			sentryFrameContextLine(frame),
		),
	}
}

func sentryFrameContextLine(frame map[string]any) string {
	line := intValue(frame, "lineno", "line", "lineNo")
	if line <= 0 {
		return ""
	}

	for _, rawContext := range arrayValue(frame["context"]) {
		contextPair := arrayValue(rawContext)
		if len(contextPair) < 2 {
			continue
		}
		if sentryFrameContextLineNumber(contextPair[0]) != line {
			continue
		}

		return anyString(contextPair[1])
	}

	return ""
}

func sentryFrameContextLineNumber(value any) int {
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
		return atoiDefault(typed)
	}

	return 0
}

func mergeSentryRequest(current Request, data map[string]any) Request {
	if data == nil {
		return current
	}

	current.Method = firstNonEmpty(current.Method, stringValue(data, "method"))
	current.URL = firstNonEmpty(current.URL, stringValue(data, "url"))
	current.Headers = mergeStringMaps(current.Headers, headersFromAny(data["headers"]))

	metadata := make(map[string]string)
	addSentryRequestQueryMetadata(metadata, data["query"])
	for _, key := range []string{"fragment", "env", "inferredContentType", "apiTarget"} {
		if value := anyString(data[key]); value != "" {
			metadata[key] = value
		}
	}
	current.Metadata = mergeStringMaps(current.Metadata, metadata)

	if current.Data == "" {
		current.Data = compactJSONOrString(data["data"])
	}

	return current
}

func addSentryRequestQueryMetadata(metadata map[string]string, value any) {
	if metadata == nil {
		return
	}

	if text := anyString(value); text != "" {
		metadata["query"] = text
		return
	}

	if root := objectValue(value); root != nil {
		for key, value := range root {
			if key = strings.TrimSpace(key); key == "" {
				continue
			}
			if text := anyString(value); text != "" {
				metadata["query."+key] = text
			}
		}

		return
	}

	for _, item := range arrayValue(value) {
		pair := arrayValue(item)
		if len(pair) < 2 {
			continue
		}

		key := anyString(pair[0])
		value := anyString(pair[1])
		if key == "" || value == "" {
			continue
		}

		metadata["query."+key] = value
	}
}

func headersFromAny(value any) map[string]string {
	switch typed := value.(type) {
	case map[string]any:
		return stringsFromMapAny(typed)
	case []any:
		out := make(map[string]string)
		for _, item := range typed {
			pair := arrayValue(item)
			if len(pair) >= 2 {
				key := anyString(pair[0])
				value := anyString(pair[1])
				if key != "" && value != "" {
					out[key] = value
				}
			}
		}

		if len(out) > 0 {
			return out
		}
	}

	return nil
}

func applySentryBreadcrumbs(inc *Context, data map[string]any) {
	for _, value := range arrayValue(data["values"]) {
		breadcrumb := objectValue(value)
		if breadcrumb == nil {
			continue
		}

		fields := stringsFromMapAny(objectValue(breadcrumb["data"]))
		inc.Logs = append(inc.Logs, Observation{
			Timestamp: timeValue(breadcrumb, "timestamp"),
			Source:    firstNonEmpty(stringValue(breadcrumb, "type"), SourceSentry),
			Name:      stringValue(breadcrumb, "category", "level"),
			Message:   stringValue(breadcrumb, "message"),
			Fields:    fields,
		})
	}
}

func compactJSONOrString(value any) string {
	if value == nil {
		return ""
	}

	if text := anyString(value); text != "" {
		return text
	}

	data, err := json.Marshal(value)
	if err != nil {
		return ""
	}

	return string(data)
}

func mergeStringMaps(left, right map[string]string) map[string]string {
	if len(left) == 0 && len(right) == 0 {
		return nil
	}

	out := make(map[string]string, len(left)+len(right))
	for key, value := range left {
		if strings.TrimSpace(key) != "" && strings.TrimSpace(value) != "" {
			out[key] = value
		}
	}
	for key, value := range right {
		if strings.TrimSpace(key) != "" && strings.TrimSpace(value) != "" {
			out[key] = value
		}
	}

	if len(out) == 0 {
		return nil
	}

	return out
}

func firstTime(values ...time.Time) time.Time {
	for _, value := range values {
		if !value.IsZero() {
			return value
		}
	}

	return time.Time{}
}
