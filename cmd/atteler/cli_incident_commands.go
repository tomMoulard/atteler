//nolint:wsl_v5 // Command flow keeps validation/fetch/render branches close for readability.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/tommoulard/atteler/pkg/autonomy"
	"github.com/tommoulard/atteler/pkg/githistory"
	"github.com/tommoulard/atteler/pkg/incident"
	"github.com/tommoulard/atteler/pkg/mcp"
	attshell "github.com/tommoulard/atteler/pkg/shell"
)

const (
	defaultIncidentTimeout       = 120 * time.Second
	defaultIncidentSentryBaseURL = "https://sentry.io"
	defaultIncidentSentryToken   = "SENTRY_AUTH_TOKEN" // #nosec G101 -- environment variable name, not a token value.
	defaultIncidentOutputBytes   = 16 * 1024
	incidentCommandStatusPassed  = "passed"
	incidentCommandStatusFailed  = "failed"
	incidentUnknownReference     = "unknown"
	maxIncidentFilenameLength    = 80
)

var defaultIncidentSentryTokenEnvNames = []string{
	defaultIncidentSentryToken,
	"SENTRY_ACCESS_TOKEN",
	"SENTRY_TOKEN",
}

var defaultIncidentGitHubTokenEnvNames = []string{
	"GH_TOKEN",
	"GITHUB_TOKEN",
	"GH_ENTERPRISE_TOKEN",
	"GITHUB_ENTERPRISE_TOKEN",
}

var incidentHTTPClient = func(timeout time.Duration) *http.Client {
	return &http.Client{Timeout: timeout}
}

type incidentFetchResult struct {
	Context  incident.Context
	Warnings []string
}

func runIncidentDiagnose(ctx context.Context, state appState, input incidentDiagnoseCommandInput) error {
	return runIncidentDiagnoseWithStateWriter(ctx, os.Stdout, state, input)
}

func runIncidentDiagnoseWithWriter(ctx context.Context, w io.Writer, cwd string, input incidentDiagnoseCommandInput) error {
	return runIncidentDiagnoseWithStateWriter(ctx, w, appState{cwd: cwd}, input)
}

func runIncidentDiagnoseWithStateWriter(ctx context.Context, w io.Writer, state appState, input incidentDiagnoseCommandInput) error {
	w = incidentOutputWriter(w)

	cwd, err := incidentWorkspaceCWD(state)
	if err != nil {
		return err
	}

	outputFormat, err := incidentDiagnosisOutputFormat(input)
	if err != nil {
		return err
	}
	err = validateIncidentDiagnoseRun(input)
	if err != nil {
		return err
	}

	timeout := incidentTimeout(input.TimeoutSeconds)
	fetched, err := fetchIncidentContext(ctx, cwd, input, timeout)
	if err != nil {
		return err
	}

	repro := runIncidentReproduction(ctx, cwd, input, timeout, fetched.Context)

	recentChanges, warnings := incidentRecentChanges(ctx, cwd, fetched.Context)
	warnings = append(warnings, fetched.Warnings...)

	analysis, err := buildIncidentAnalysis(ctx, cwd, fetched.Context, recentChanges, nil, repro, nil, warnings)
	if err != nil {
		return err
	}

	analysis, err = runIncidentRepairAndValidation(ctx, state, cwd, input, timeout, fetched.Context, recentChanges, nil, repro, warnings, analysis)
	if err != nil {
		return err
	}

	artifactWarnings, artifactErr := writeIncidentArtifacts(cwd, analysis, input)
	if artifactErr != nil {
		return artifactErr
	}
	analysis.Warnings = append(analysis.Warnings, artifactWarnings...)

	if input.OpenPR {
		prURL, openErr := openIncidentPR(ctx, cwd, analysis, input, timeout)
		if openErr != nil {
			return openErr
		}
		if prURL != "" {
			analysis.Warnings = append(analysis.Warnings, "GitHub PR created: "+prURL)
		}
	}

	return writeIncidentDiagnosisOutput(w, outputFormat, analysis)
}

func incidentOutputWriter(w io.Writer) io.Writer {
	if w == nil {
		return io.Discard
	}

	return w
}

func incidentWorkspaceCWD(state appState) (string, error) {
	cwd := strings.TrimSpace(state.cwd)
	if cwd != "" {
		return cwd, nil
	}

	cwd, err := os.Getwd()
	if err != nil {
		return "", fmt.Errorf("incident diagnose: locate working directory: %w", err)
	}

	return cwd, nil
}

func incidentDiagnosisOutputFormat(input incidentDiagnoseCommandInput) (string, error) {
	if input.JSON {
		input.OutputFormat = outputFormatJSON
	}

	outputFormat, err := normalizeIncidentOutputFormat(input.OutputFormat)
	if err != nil {
		return "", err
	}
	if input.ApplyFix && outputFormat != outputFormatText {
		return "", errors.New("incident apply fix currently supports --output text only because the agent repair loop streams its own response")
	}

	return outputFormat, nil
}

func validateIncidentDiagnoseRun(input incidentDiagnoseCommandInput) error {
	if input.OpenPR && !input.ApplyFix {
		return errors.New("--incident-open-pr requires --incident-apply-fix so the PR is created only after a repair attempt; use --incident-pr-body for a diagnose-only PR template")
	}
	if input.OpenPR && len(input.ValidationCommands) == 0 {
		return errors.New("--incident-open-pr requires at least one --incident-validation-command so the PR includes harness-captured validation evidence")
	}

	return nil
}

func runIncidentRepairAndValidation(
	ctx context.Context,
	state appState,
	cwd string,
	input incidentDiagnoseCommandInput,
	timeout time.Duration,
	inc incident.Context,
	recentChanges []incident.Change,
	worktreeChanges []incident.WorktreeChange,
	repro incident.CommandResult,
	warnings []string,
	analysis incident.Analysis,
) (incident.Analysis, error) {
	if input.ApplyFix {
		baselineChanges, baselineWarnings := incidentWorktreeChanges(ctx, cwd)
		warnings = append(warnings, baselineWarnings...)

		fixErr := runIncidentFixLoop(ctx, state, analysis)
		if fixErr != nil {
			return incident.Analysis{}, fixErr
		}

		var changeWarnings []string
		worktreeChanges, changeWarnings = incidentWorktreeChanges(ctx, cwd)
		warnings = append(warnings, changeWarnings...)
		if len(baselineChanges) > 0 {
			filteredChanges := incidentNewWorktreeChanges(worktreeChanges, baselineChanges)
			if len(filteredChanges) < len(worktreeChanges) {
				warnings = append(warnings, "worktree had pre-existing changes before repair; code-change summary omits unchanged pre-existing status/path entries")
			}
			worktreeChanges = filteredChanges
		}
	}

	validation := runIncidentValidation(ctx, cwd, input, timeout, inc)
	if len(validation) == 0 && !input.ApplyFix {
		return analysis, nil
	}

	return buildIncidentAnalysis(ctx, cwd, inc, recentChanges, worktreeChanges, repro, validation, warnings)
}

func writeIncidentDiagnosisOutput(w io.Writer, outputFormat string, analysis incident.Analysis) error {
	analysis = incident.RedactAnalysis(analysis)

	switch outputFormat {
	case outputFormatJSON:
		data, marshalErr := json.MarshalIndent(analysis, "", "  ")
		if marshalErr != nil {
			return fmt.Errorf("incident diagnose: marshal JSON: %w", marshalErr)
		}
		_, err := fmt.Fprintln(w, string(data))
		if err != nil {
			return fmt.Errorf("incident diagnose: write output: %w", err)
		}
	case commandOutputMarkdown:
		_, err := fmt.Fprint(w, incident.RenderMarkdown(analysis))
		if err != nil {
			return fmt.Errorf("incident diagnose: write output: %w", err)
		}
	default:
		_, err := fmt.Fprint(w, incident.RenderText(analysis))
		if err != nil {
			return fmt.Errorf("incident diagnose: write output: %w", err)
		}
	}

	return nil
}

func buildIncidentAnalysis(
	ctx context.Context,
	cwd string,
	inc incident.Context,
	recentChanges []incident.Change,
	worktreeChanges []incident.WorktreeChange,
	repro incident.CommandResult,
	validation []incident.CommandResult,
	warnings []string,
) (incident.Analysis, error) {
	analysis, err := incident.Analyze(ctx, inc, incident.AnalysisOptions{
		RepoRoot:        cwd,
		RecentChanges:   recentChanges,
		WorktreeChanges: worktreeChanges,
		Reproduction:    repro,
		Validation:      validation,
		MaxFrames:       12,
		MaxSourceFiles:  20_000,
	})
	if err != nil {
		return incident.Analysis{}, fmt.Errorf("incident diagnose: analyze incident: %w", err)
	}
	analysis.Warnings = append(analysis.Warnings, warnings...)

	return analysis, nil
}

func fetchIncidentContext(ctx context.Context, cwd string, input incidentDiagnoseCommandInput, timeout time.Duration) (incidentFetchResult, error) {
	if strings.TrimSpace(input.FilePath) != "" {
		input.FilePath = resolveWorkspacePath(cwd, input.FilePath)
		return fetchIncidentFromFile(input)
	}
	if strings.TrimSpace(input.MCPManifestPath) != "" {
		input.MCPManifestPath = resolveWorkspacePath(cwd, input.MCPManifestPath)
	}

	switch {
	case strings.TrimSpace(input.SentryIssue) != "":
		return fetchSentryIncident(ctx, input, timeout)
	case strings.TrimSpace(input.MCPManifestPath) != "" || strings.TrimSpace(input.MCPToolName) != "":
		return fetchMCPIncident(ctx, input, timeout)
	default:
		return incidentFetchResult{}, errors.New("incident diagnose: provide --sentry, --incident-file, or --incident-mcp-manifest/--incident-mcp-tool")
	}
}

func fetchIncidentFromFile(input incidentDiagnoseCommandInput) (incidentFetchResult, error) {
	data, err := os.ReadFile(input.FilePath)
	if err != nil {
		return incidentFetchResult{}, fmt.Errorf("incident file: read %s: %s", incident.RedactIdentifier(input.FilePath), redactedIncidentError(err))
	}

	source := incident.SourceFile
	reference := input.Reference
	if input.SentryIssue != "" {
		source = incident.SourceSentry
		reference = input.SentryIssue
	}

	ctx, err := incident.ParseJSONIncident(data, source, reference)
	if err != nil {
		return incidentFetchResult{}, fmt.Errorf("incident file: %w", err)
	}

	return incidentFetchResult{Context: ctx}, nil
}

func fetchSentryIncident(ctx context.Context, input incidentDiagnoseCommandInput, timeout time.Duration) (incidentFetchResult, error) {
	orgFromRef, issueID := sentryReferenceParts(input.SentryIssue)
	org := firstNonEmpty(input.SentryOrg, orgFromRef, os.Getenv("SENTRY_ORG"))
	if strings.TrimSpace(org) == "" {
		return incidentFetchResult{}, errors.New("incident sentry: --incident-sentry-org or SENTRY_ORG is required")
	}
	if strings.TrimSpace(issueID) == "" {
		return incidentFetchResult{}, errors.New("incident sentry: issue reference is required")
	}

	_, token := sentryAuthToken(input.SentryTokenEnv)
	if token == "" {
		return incidentFetchResult{}, fmt.Errorf("incident sentry: %s is required (override with --incident-sentry-token-env)", sentryTokenEnvRequirement(input.SentryTokenEnv))
	}

	baseURL := sentryBaseURLForReference(input.SentryBaseURL, input.SentryIssue)
	client := incidentHTTPClient(timeout)

	canonicalIssueID, lookupRaw, lookupErr := resolveSentryIssueReference(ctx, client, baseURL, org, issueID, token)
	if lookupErr != nil {
		return incidentFetchResult{}, lookupErr
	}
	if canonicalIssueID != "" {
		issueID = canonicalIssueID
	}
	eventID := firstNonEmpty(input.SentryEventID, sentryEventIDFromReference(input.SentryIssue))

	issueURL := sentryIssueEndpoint(baseURL, org, issueID)

	issueRaw, err := sentryGET(ctx, client, issueURL, token)
	if err != nil {
		if len(lookupRaw) == 0 {
			return incidentFetchResult{}, fmt.Errorf("incident sentry: fetch issue: %w", err)
		}

		issueRaw = lookupRaw
	}

	eventRaw, eventErr := sentryEventPayload(ctx, client, baseURL, org, issueID, eventID, token)
	var warnings []string
	if eventErr != nil {
		warnings = append(warnings, eventErr.Error())
	}

	inc, err := incident.ParseSentryIncident(issueRaw, eventRaw)
	if err != nil {
		return incidentFetchResult{}, fmt.Errorf("incident sentry: parse issue: %w", err)
	}
	inc.Reference = firstNonEmpty(inc.Reference, issueID, input.SentryIssue)

	return incidentFetchResult{Context: inc, Warnings: warnings}, nil
}

func sentryAuthToken(configuredEnv string) (envName, token string) {
	if configuredEnv = strings.TrimSpace(configuredEnv); configuredEnv != "" {
		return configuredEnv, strings.TrimSpace(os.Getenv(configuredEnv))
	}

	for _, name := range defaultIncidentSentryTokenEnvNames {
		if token = strings.TrimSpace(os.Getenv(name)); token != "" {
			return name, token
		}
	}

	return defaultIncidentSentryToken, ""
}

func sentryTokenEnvRequirement(configuredEnv string) string {
	if configuredEnv = strings.TrimSpace(configuredEnv); configuredEnv != "" {
		return incident.RedactIdentifier(configuredEnv)
	}

	names := make([]string, 0, len(defaultIncidentSentryTokenEnvNames))
	for _, name := range defaultIncidentSentryTokenEnvNames {
		names = append(names, incident.RedactIdentifier(name))
	}

	return "one of " + strings.Join(names, ", ")
}

func sentryBaseURL(configuredURL string) string {
	baseURL := firstNonEmpty(configuredURL, os.Getenv("SENTRY_BASE_URL"), os.Getenv("SENTRY_HOST"), defaultIncidentSentryBaseURL)

	return normalizeSentryBaseURL(baseURL)
}

func sentryBaseURLForReference(configuredURL, reference string) string {
	baseURL := firstNonEmpty(configuredURL, os.Getenv("SENTRY_BASE_URL"), os.Getenv("SENTRY_HOST"), sentryReferenceBaseURL(reference), defaultIncidentSentryBaseURL)

	return normalizeSentryBaseURL(baseURL)
}

func normalizeSentryBaseURL(baseURL string) string {
	if !strings.Contains(baseURL, "://") {
		baseURL = "https://" + baseURL
	}

	return strings.TrimRight(baseURL, "/")
}

func sentryReferenceBaseURL(raw string) string {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || parsed.Host == "" {
		return ""
	}
	if parsed.Scheme == "" {
		parsed.Scheme = "https"
	}

	return parsed.Scheme + "://" + parsed.Host
}

func resolveSentryIssueReference(ctx context.Context, client *http.Client, baseURL, org, issueID, token string) (canonicalIssueID string, issueRaw []byte, err error) {
	if sentryIssueIDIsNumeric(issueID) {
		return issueID, nil, nil
	}

	canonicalIssueID, issueRaw, err = resolveSentryIssueReferenceBySearch(ctx, client, baseURL, org, issueID, token)
	if err == nil {
		return canonicalIssueID, issueRaw, nil
	}

	raw, shortIDErr := sentryGET(ctx, client, sentryShortIDEndpoint(baseURL, org, issueID), token)
	if shortIDErr == nil {
		return sentryShortIDLookupResult(raw, issueID)
	}

	return "", nil, err
}

func resolveSentryIssueReferenceBySearch(ctx context.Context, client *http.Client, baseURL, org, issueID, token string) (canonicalIssueID string, issueRaw []byte, err error) {
	endpoint := sentryIssuesSearchEndpoint(baseURL, org, issueID)
	raw, err := sentryGET(ctx, client, endpoint, token)
	if err != nil {
		return "", nil, fmt.Errorf("incident sentry: resolve short issue id: %w", err)
	}

	var issues []map[string]any
	if err := json.Unmarshal(raw, &issues); err != nil {
		return "", nil, fmt.Errorf("incident sentry: parse short issue id lookup: %w", err)
	}
	if len(issues) == 0 {
		return "", nil, fmt.Errorf("incident sentry: resolve short issue id %q: no matching issues", incident.RedactIdentifier(issueID))
	}

	match := issues[0]
	for _, candidate := range issues {
		if strings.EqualFold(sentryLookupString(candidate, "shortId", "short_id"), issueID) || sentryLookupString(candidate, "id") == issueID {
			match = candidate
			break
		}
	}

	canonicalID := sentryLookupString(match, "id")
	if canonicalID == "" {
		return "", nil, fmt.Errorf("incident sentry: resolve short issue id %q: response missing numeric id", incident.RedactIdentifier(issueID))
	}

	return canonicalID, mustJSONMap(match), nil
}

func sentryShortIDLookupResult(raw []byte, issueID string) (canonicalIssueID string, issueRaw []byte, err error) {
	var result map[string]any
	if err := json.Unmarshal(raw, &result); err != nil {
		return "", nil, fmt.Errorf("incident sentry: parse short issue id lookup: %w", err)
	}

	group := sentryLookupMap(result, "group", "issue")
	canonicalID := firstNonEmpty(
		sentryLookupString(result, "groupId", "groupID", "id"),
		sentryLookupString(group, "id"),
	)
	if canonicalID == "" {
		return "", nil, fmt.Errorf("incident sentry: resolve short issue id %q: response missing numeric id", incident.RedactIdentifier(issueID))
	}

	if len(group) > 0 {
		return canonicalID, mustJSONMap(group), nil
	}

	return canonicalID, raw, nil
}

func fetchMCPIncident(ctx context.Context, input incidentDiagnoseCommandInput, timeout time.Duration) (incidentFetchResult, error) {
	if strings.TrimSpace(input.MCPManifestPath) == "" {
		return incidentFetchResult{}, errors.New("incident mcp: --incident-mcp-manifest is required")
	}
	if strings.TrimSpace(input.MCPServerName) == "" {
		return incidentFetchResult{}, errors.New("incident mcp: --incident-mcp-server is required")
	}
	if strings.TrimSpace(input.MCPToolName) == "" {
		return incidentFetchResult{}, errors.New("incident mcp: --incident-mcp-tool is required")
	}

	manifest, err := loadMCPManifest(ctx, input.MCPManifestPath)
	if err != nil {
		return incidentFetchResult{}, fmt.Errorf("incident mcp: %s", incident.RedactText(err.Error()))
	}
	validateErr := manifest.Validate()
	if validateErr != nil {
		return incidentFetchResult{}, fmt.Errorf("incident mcp: validate manifest: %s", incident.RedactText(validateErr.Error()))
	}

	server, ok := findMCPServer(manifest, input.MCPServerName)
	if !ok {
		return incidentFetchResult{}, fmt.Errorf("incident mcp: server %q not found", incident.RedactIdentifier(strings.TrimSpace(input.MCPServerName)))
	}

	args, err := parseMCPToolArgs(input.MCPToolArgsJSON)
	if err != nil {
		return incidentFetchResult{}, err
	}
	if args == nil {
		args = make(map[string]any)
	}
	if strings.TrimSpace(input.Reference) != "" {
		args["reference"] = input.Reference
	}
	argSecrets := incident.SensitiveValues(input.MCPToolArgsJSON)

	response, err := mcp.CallTool(ctx, server, input.MCPToolName, args, timeout)
	if err != nil {
		return incidentFetchResult{}, fmt.Errorf("incident mcp: %s", redactedIncidentTextWithSensitiveValues(err.Error(), argSecrets))
	}
	if message := incidentMCPToolResultError(response.Result); message != "" {
		return incidentFetchResult{}, fmt.Errorf("incident mcp: tool returned error result: %s", redactedIncidentTextWithSensitiveValues(message, argSecrets))
	}

	payload := incidentPayloadFromMCPResult(response.Result, input.Reference)
	inc, err := incident.ParseJSONIncident(payload, incident.SourceMCP, input.Reference)
	if err != nil {
		return incidentFetchResult{}, fmt.Errorf("incident mcp: parse tool result: %w", err)
	}

	return incidentFetchResult{Context: inc}, nil
}

func incidentMCPToolResultError(raw json.RawMessage) string {
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return ""
	}

	if !mcpResultIsError(envelope) {
		return ""
	}

	messages := make([]string, 0)
	for _, content := range mcpContentItems(envelope["content"]) {
		if text := mcpResultErrorContentText(content); text != "" {
			messages = append(messages, text)
		}
	}
	if len(messages) == 0 {
		return "MCP tool returned isError=true without a text error message"
	}

	return strings.Join(messages, "\n")
}

func mcpResultIsError(envelope map[string]json.RawMessage) bool {
	for _, key := range []string{"isError", "is_error"} {
		var isError bool
		if err := json.Unmarshal(envelope[key], &isError); err == nil && isError {
			return true
		}
	}

	return false
}

func mcpResultErrorContentText(content json.RawMessage) string {
	switch strings.ToLower(mcpContentString(content, "type")) {
	case commandOutputText:
		return mcpContentString(content, commandOutputText)
	case "resource":
		return mcpContentString(mcpContentField(content, "resource"), commandOutputText)
	default:
		return ""
	}
}

//nolint:gosec // Endpoint is an explicit user-configured observability integration target; secrets are redacted in errors.
func sentryGET(ctx context.Context, client *http.Client, endpoint, token string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, http.NoBody)
	if err != nil {
		return nil, fmt.Errorf("incident sentry: build request: %s", redactedIncidentError(err))
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("incident sentry: fetch %s: %s", incident.RedactIdentifier(endpoint), redactedSentryError(err, token))
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, fmt.Errorf("incident sentry: read response: %s", redactedIncidentError(err))
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("incident sentry: fetch %s: status %s: %s", incident.RedactIdentifier(endpoint), resp.Status, redactedSentryText(string(body), token))
	}

	return body, nil
}

func redactedSentryError(err error, token string) string {
	if err == nil {
		return ""
	}

	return redactedSentryText(err.Error(), token)
}

func redactedSentryText(text, token string) string {
	if token = strings.TrimSpace(token); token != "" {
		text = strings.ReplaceAll(text, token, "[REDACTED:sentry_token]")
	}

	return incident.RedactText(text)
}

func sentryEventPayload(ctx context.Context, client *http.Client, baseURL, org, issueID, eventID, token string) ([]byte, error) {
	endpoint := sentryEventsEndpoint(baseURL, org, issueID, eventID)
	raw, err := sentryGET(ctx, client, endpoint, token)
	if err != nil {
		return nil, fmt.Errorf("incident sentry: event details unavailable: %w", err)
	}

	if strings.TrimSpace(eventID) != "" {
		return raw, nil
	}

	var events []json.RawMessage
	if err := json.Unmarshal(raw, &events); err != nil {
		var event map[string]any
		if objectErr := json.Unmarshal(raw, &event); objectErr == nil && len(event) > 0 {
			return raw, nil
		}

		return nil, fmt.Errorf("incident sentry: parse issue events list: %w", err)
	}
	if len(events) == 0 {
		return nil, errors.New("incident sentry: issue has no events")
	}

	return events[0], nil
}

func sentryReferenceParts(raw string) (org, issueID string) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", ""
	}

	parsed, err := url.Parse(raw)
	if err == nil && parsed.Host != "" {
		org = firstNonEmpty(org, sentryOrgFromHost(parsed.Host))
		parts := splitPathSegments(parsed.Path)
		for i, part := range parts {
			switch part {
			case "organizations":
				if i+1 < len(parts) {
					org = parts[i+1]
				}
			case "issues":
				if i+1 < len(parts) {
					issueID = parts[i+1]
				}
			}
		}
		if issueID != "" {
			return org, issueID
		}
	}

	parts := splitPathSegments(raw)
	if len(parts) == 2 {
		return parts[0], parts[1]
	}

	return "", strings.Trim(raw, "/")
}

func sentryOrgFromHost(host string) string {
	host = strings.ToLower(strings.TrimSpace(host))
	if before, _, ok := strings.Cut(host, ":"); ok {
		host = before
	}

	const sentryHostedSuffix = ".sentry.io"
	if !strings.HasSuffix(host, sentryHostedSuffix) {
		return ""
	}

	org := strings.TrimSuffix(host, sentryHostedSuffix)
	if org == "" || strings.Contains(org, ".") {
		return ""
	}

	return org
}

func sentryEventIDFromReference(raw string) string {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || parsed.Host == "" {
		return ""
	}

	parts := splitPathSegments(parsed.Path)
	for i, part := range parts {
		if part == "events" && i+1 < len(parts) {
			return parts[i+1]
		}
	}

	return ""
}

func splitPathSegments(path string) []string {
	var out []string
	for part := range strings.SplitSeq(path, "/") {
		if part = strings.TrimSpace(part); part != "" {
			out = append(out, part)
		}
	}

	return out
}

func sentryIssueEndpoint(baseURL, org, issueID string) string {
	return strings.TrimRight(baseURL, "/") +
		"/api/0/organizations/" + url.PathEscape(org) +
		"/issues/" + url.PathEscape(issueID) + "/"
}

func sentryShortIDEndpoint(baseURL, org, issueID string) string {
	return strings.TrimRight(baseURL, "/") +
		"/api/0/organizations/" + url.PathEscape(org) +
		"/shortids/" + url.PathEscape(issueID) + "/"
}

func sentryEventsEndpoint(baseURL, org, issueID, eventID string) string {
	base := sentryIssueEndpoint(baseURL, org, issueID) + "events/"
	if strings.TrimSpace(eventID) != "" {
		return base + url.PathEscape(eventID) + "/"
	}

	return base + "?full=true&limit=1"
}

func sentryIssuesSearchEndpoint(baseURL, org, reference string) string {
	values := url.Values{}
	values.Set("shortIdLookup", "1")
	values.Set("query", strings.TrimSpace(reference))
	values.Set("limit", "1")

	return strings.TrimRight(baseURL, "/") +
		"/api/0/organizations/" + url.PathEscape(org) +
		"/issues/?" + values.Encode()
}

func sentryIssueIDIsNumeric(issueID string) bool {
	issueID = strings.TrimSpace(issueID)
	if issueID == "" {
		return false
	}

	for _, r := range issueID {
		if r < '0' || r > '9' {
			return false
		}
	}

	return true
}

func sentryLookupString(root map[string]any, keys ...string) string {
	for _, key := range keys {
		switch value := root[key].(type) {
		case string:
			if strings.TrimSpace(value) != "" {
				return strings.TrimSpace(value)
			}
		case float64:
			return strings.TrimRight(strings.TrimRight(fmt.Sprintf("%.6f", value), "0"), ".")
		}
	}

	return ""
}

func sentryLookupMap(root map[string]any, keys ...string) map[string]any {
	for _, key := range keys {
		if value, ok := root[key].(map[string]any); ok {
			return value
		}
	}

	return nil
}

func mustJSONMap(value map[string]any) []byte {
	data, err := json.Marshal(value)
	if err != nil {
		return nil
	}

	return data
}

func incidentPayloadFromMCPResult(raw json.RawMessage, reference string) []byte {
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return raw
	}

	if payload := incidentPayloadFromMCPJSONPayload(raw, reference, firstMCPEnvelopeField(envelope, "structuredContent", "structured_content")); payload != nil {
		return payload
	}

	contentItems := mcpContentItems(envelope["content"])
	for _, content := range contentItems {
		if payload := incidentPayloadFromMCPJSONPayload(raw, reference, mcpContentField(content, "json")); payload != nil {
			return payload
		}
	}

	for _, content := range contentItems {
		if payload := incidentPayloadFromMCPText(raw, reference, content); payload != nil {
			return payload
		}
	}

	for _, content := range contentItems {
		if payload := incidentPayloadFromMCPResourceText(raw, reference, content); payload != nil {
			return payload
		}
	}

	return raw
}

func firstMCPEnvelopeField(envelope map[string]json.RawMessage, keys ...string) json.RawMessage {
	for _, key := range keys {
		if payload := envelope[key]; len(payload) > 0 {
			return payload
		}
	}

	return nil
}

func mcpContentItems(raw json.RawMessage) []json.RawMessage {
	var contentItems []json.RawMessage
	if err := json.Unmarshal(raw, &contentItems); err != nil {
		return nil
	}

	return contentItems
}

func incidentPayloadFromMCPJSONPayload(raw json.RawMessage, reference string, payload json.RawMessage) []byte {
	if jsonObjectPayload(payload) {
		return payload
	}
	if jsonArrayPayload(payload) {
		return mcpArrayIncidentPayload(raw, reference, payload)
	}

	return nil
}

func incidentPayloadFromMCPText(raw json.RawMessage, reference string, content json.RawMessage) []byte {
	text := mcpContentString(content, commandOutputText)
	if !strings.EqualFold(mcpContentString(content, "type"), commandOutputText) || text == "" {
		return nil
	}

	return incidentPayloadFromMCPTextValue(raw, reference, text)
}

func incidentPayloadFromMCPResourceText(raw json.RawMessage, reference string, content json.RawMessage) []byte {
	if !strings.EqualFold(mcpContentString(content, "type"), "resource") {
		return nil
	}

	text := mcpContentString(mcpContentField(content, "resource"), commandOutputText)
	if text == "" {
		return nil
	}

	return incidentPayloadFromMCPTextValue(raw, reference, text)
}

func incidentPayloadFromMCPTextValue(raw json.RawMessage, reference, text string) []byte {
	if jsonObjectPayload(json.RawMessage(text)) {
		return []byte(text)
	}
	if jsonArrayPayload(json.RawMessage(text)) {
		return mcpArrayIncidentPayload(raw, reference, json.RawMessage(text))
	}

	return mcpTextIncidentPayload(raw, reference, text)
}

func mcpContentField(raw json.RawMessage, field string) json.RawMessage {
	var root map[string]json.RawMessage
	if err := json.Unmarshal(raw, &root); err != nil {
		return nil
	}

	return root[field]
}

func mcpContentString(raw json.RawMessage, field string) string {
	var value string
	if err := json.Unmarshal(mcpContentField(raw, field), &value); err != nil {
		return ""
	}

	return strings.TrimSpace(value)
}

func jsonObjectPayload(raw json.RawMessage) bool {
	return strings.HasPrefix(strings.TrimSpace(string(raw)), "{")
}

func jsonArrayPayload(raw json.RawMessage) bool {
	return strings.HasPrefix(strings.TrimSpace(string(raw)), "[")
}

func mcpTextIncidentPayload(raw json.RawMessage, reference, text string) []byte {
	payload, err := json.Marshal(map[string]string{
		"source":    incident.SourceMCP,
		"reference": reference,
		"message":   text,
	})
	if err != nil {
		return raw
	}

	return payload
}

func mcpArrayIncidentPayload(raw json.RawMessage, reference string, items json.RawMessage) []byte {
	payload, err := json.Marshal(map[string]any{
		"source":    incident.SourceMCP,
		"reference": reference,
		"logs":      items,
	})
	if err != nil {
		return raw
	}

	return payload
}

func incidentRecentChanges(ctx context.Context, cwd string, inc incident.Context) (changes []incident.Change, warnings []string) {
	commits, err := gitHistoryCommits(ctx, cwd, autonomy.Medium, gitHistoryCollectOptions{
		Audit: attshell.AuditContext{
			Caller:          "atteler.incident.git_history",
			IssueID:         incident.RedactIdentifier(inc.Reference),
			IssueIdentifier: incident.RedactIdentifier(inc.Source),
		},
		RedactOutput: incident.RedactText,
		OutputNote:   "incident git history output redacted before audit capture",
	})
	if err != nil {
		return nil, []string{"git history correlation unavailable: " + incident.RedactText(err.Error())}
	}

	idx := githistory.NewIndex(commits)
	queries := incidentHistoryQueries(inc)
	seen := make(map[string]bool)
	changes = make([]incident.Change, 0)
	for _, query := range queries {
		results := idx.Search(query, incidentHistorySearchLimit(query))
		for i := range results {
			result := results[i]
			if !incidentHistoryResultMatchesQuery(result, query) {
				continue
			}

			hash := result.Commit.Hash
			if seen[hash] {
				continue
			}
			seen[hash] = true
			changes = append(changes, incidentHistoryChange(result, query))
			if len(changes) >= 5 {
				return changes, nil
			}
		}
	}

	return changes, nil
}

func incidentHistoryChange(result githistory.Result, query string) incident.Change {
	subject := incident.RedactText(result.Commit.Subject)
	files := make([]string, 0, len(result.Commit.Files))
	for _, file := range result.Commit.Files {
		if file = incident.RedactIdentifier(file); strings.TrimSpace(file) != "" {
			files = append(files, file)
		}
	}

	return incident.Change{
		Hash:         incident.RedactIdentifier(result.Commit.Hash),
		Date:         result.Commit.Date,
		Author:       incident.RedactText(result.Commit.AuthorName),
		Subject:      subject,
		Files:        files,
		Match:        incident.RedactText(query),
		PullRequests: incident.PullRequestsFromText(subject),
	}
}

func incidentHistorySearchLimit(query string) int {
	if incidentHistoryQueryLooksPath(query) {
		return 10
	}

	return 2
}

func incidentHistoryResultMatchesQuery(result githistory.Result, query string) bool {
	if !incidentHistoryQueryLooksPath(query) {
		return true
	}

	return incidentHistoryFileMatchesQuery(result.Commit.Files, query)
}

func incidentHistoryFileMatchesQuery(files []string, query string) bool {
	query = normalizeIncidentHistoryPath(query)
	if query == "" {
		return false
	}

	for _, file := range files {
		file = normalizeIncidentHistoryPath(file)
		if file == "" {
			continue
		}
		if file == query || strings.HasSuffix(query, "/"+file) || strings.HasSuffix(file, "/"+query) {
			return true
		}
		if !strings.Contains(query, "/") && filepath.Base(file) == query {
			return true
		}
	}

	return false
}

func incidentHistoryQueryLooksPath(query string) bool {
	query = strings.TrimSpace(query)
	if strings.ContainsAny(query, `/\`) {
		return true
	}

	switch strings.ToLower(filepath.Ext(query)) {
	case ".c", ".cc", ".cpp", ".cs", ".erl", ".ex", ".exs", ".go", ".h", ".hpp", ".java", ".js", ".jsx", ".kt", ".m", ".mm", ".php", ".py", ".rb", ".rs", ".scala", ".sh", ".swift", ".ts", ".tsx":
		return true
	default:
		return false
	}
}

func normalizeIncidentHistoryPath(path string) string {
	path = strings.TrimSpace(path)
	path = strings.TrimPrefix(path, "file://")
	path = strings.ReplaceAll(path, "\\", "/")
	path = strings.TrimPrefix(path, "./")
	return strings.Trim(path, "/")
}

func incidentWorktreeChanges(ctx context.Context, cwd string) (changes []incident.WorktreeChange, warnings []string) {
	var stdout, stderr bytes.Buffer
	cmd, invocation, err := attshell.CommandContext(ctx, attshell.CommandOptions{
		Program: "git",
		Args:    []string{"status", "--short"},
		Dir:     cwd,
		Stdout:  &stdout,
		Stderr:  &stderr,
		Mode:    attshell.ModeCaptured,
		Audit:   attshell.AuditContext{Caller: "atteler.incident.worktree"},
	})
	if err != nil {
		return nil, []string{"worktree change summary unavailable: " + incident.RedactText(err.Error())}
	}

	runErr := cmd.Run()
	finishErr := invocation.Finish(attshell.FinishOptions{
		Stdout:        incident.RedactText(stdout.String()),
		Stderr:        incident.RedactText(stderr.String()),
		Error:         runErr,
		OutputCapture: attshell.OutputCaptured,
		OutputNote:    "incident worktree status output redacted before audit capture",
	})
	if finishErr != nil {
		return nil, []string{"worktree change summary unavailable: " + incident.RedactText(finishErr.Error())}
	}
	if runErr != nil {
		return nil, []string{"worktree change summary unavailable: " + incident.RedactText(runErr.Error())}
	}

	return parseIncidentWorktreeStatus(stdout.String()), nil
}

func parseIncidentWorktreeStatus(raw string) []incident.WorktreeChange {
	var changes []incident.WorktreeChange
	for line := range strings.Lines(raw) {
		line = strings.TrimRight(line, "\r\n")
		if strings.TrimSpace(line) == "" {
			continue
		}

		status, path := incidentWorktreeStatusLine(line)
		if path == "" {
			continue
		}

		changes = append(changes, incident.WorktreeChange{Status: status, Path: path})
		if len(changes) >= 50 {
			break
		}
	}

	return changes
}

func incidentNewWorktreeChanges(after, before []incident.WorktreeChange) []incident.WorktreeChange {
	if len(before) == 0 {
		return after
	}

	beforeCounts := make(map[string]int, len(before))
	for i := range before {
		beforeCounts[incidentWorktreeChangeKey(before[i])]++
	}

	out := make([]incident.WorktreeChange, 0, len(after))
	for i := range after {
		key := incidentWorktreeChangeKey(after[i])
		if beforeCounts[key] > 0 {
			beforeCounts[key]--
			continue
		}

		out = append(out, after[i])
	}

	return out
}

func incidentWorktreeChangeKey(change incident.WorktreeChange) string {
	return strings.TrimSpace(change.Status) + "\x00" + strings.TrimSpace(change.Path)
}

func incidentWorktreeStatusLine(line string) (status, path string) {
	if len(line) < 3 {
		return strings.TrimSpace(line), ""
	}

	status = strings.TrimSpace(line[:2])
	path = strings.TrimSpace(line[2:])
	if after, ok := strings.CutPrefix(path, "-> "); ok {
		path = after
	} else if _, after, ok := strings.Cut(path, " -> "); ok {
		path = after
	}

	return status, path
}

func incidentHistoryQueries(inc incident.Context) []string {
	seen := make(map[string]bool)
	var queries []string
	for i := range inc.StackTrace {
		frame := inc.StackTrace[i]
		for _, value := range []string{frame.File, frame.AbsPath, frame.Function, frame.Module} {
			value = strings.TrimSpace(value)
			if value != "" && !seen[value] {
				seen[value] = true
				queries = append(queries, value)
			}
		}
	}
	for _, value := range []string{inc.ErrorType, inc.Message, inc.Service, inc.Release, inc.Commit} {
		value = strings.TrimSpace(value)
		if value != "" && !seen[value] {
			seen[value] = true
			queries = append(queries, value)
		}
	}
	for i := range inc.Deployments {
		for _, value := range []string{inc.Deployments[i].Version, inc.Deployments[i].Commit, inc.Deployments[i].Environment} {
			value = strings.TrimSpace(value)
			if value != "" && !seen[value] {
				seen[value] = true
				queries = append(queries, value)
			}
		}
	}

	if len(queries) > 8 {
		return queries[:8]
	}

	return queries
}

func runIncidentReproduction(ctx context.Context, cwd string, input incidentDiagnoseCommandInput, timeout time.Duration, inc incident.Context) incident.CommandResult {
	command := strings.TrimSpace(input.ReproCommand)
	if command == "" {
		return incident.CommandResult{}
	}

	return runIncidentLocalCommand(ctx, cwd, command, timeout, inc)
}

func runIncidentValidation(ctx context.Context, cwd string, input incidentDiagnoseCommandInput, timeout time.Duration, inc incident.Context) []incident.CommandResult {
	results := make([]incident.CommandResult, 0, len(input.ValidationCommands))
	for _, command := range input.ValidationCommands {
		if strings.TrimSpace(command) == "" {
			continue
		}

		results = append(results, runIncidentLocalCommand(ctx, cwd, command, timeout, inc))
	}

	return results
}

func runIncidentFixLoop(ctx context.Context, state appState, analysis incident.Analysis) error {
	if state.registry == nil {
		return errors.New("incident apply fix: LLM registry is required")
	}
	if state.agentRegistry == nil {
		return errors.New("incident apply fix: agent registry is required")
	}
	if state.sessionStore == nil {
		return errors.New("incident apply fix: session store is required")
	}

	fmt.Fprintln(os.Stderr, "incident: running selected Atteler agent with redacted fix prompt")

	return runOnceWithOptions(
		ctx,
		state.registry,
		state.agentRegistry,
		state.hookRunner,
		state.sessionStore,
		state.sessionState,
		state.contextOptions,
		state.referenceContext,
		state.referenceManifest,
		state.referenceContextEstimator,
		state.configuredReferences,
		state.selectedModel,
		state.selectedAgent,
		state.fallbackModels,
		state.generationDefaults,
		state.generationOverrides,
		state.maxInputTokens,
		runOnceExecutionOptions{
			OutputFormat:                outputFormatText,
			AgentLoopBudget:             state.agentLoopBudget,
			AgentLoopCheckpointInterval: state.agentLoopCheckpointInterval,
			SkillLearningStoreDir:       state.skillLearningStoreDir,
			SkillLearningSkillDir:       state.skillLearningSkillDir,
			SkillLearningEnabled:        state.skillLearningEnabled,
			VectorConfig:                state.vectorConfig,
		},
		state.modelLocked,
		incident.BuildFixPrompt(analysis),
	)
}

func runIncidentLocalCommand(ctx context.Context, cwd, command string, timeout time.Duration, inc incident.Context) incident.CommandResult {
	started := time.Now().UTC()
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	stdout, stderr, outputLimit := incidentCommandOutputBuffers(defaultIncidentOutputBytes)
	cmd, invocation, err := attshell.CommandContext(runCtx, attshell.CommandOptions{
		Program:      "bash",
		Args:         []string{"--noprofile", "--norc", "-lc", command},
		Command:      command,
		Dir:          cwd,
		Stdout:       stdout,
		Stderr:       stderr,
		Mode:         attshell.ModeCaptured,
		Policy:       &attshell.Policy{DenyNetwork: true},
		SecretValues: incident.SensitiveValues(command),
		Audit: attshell.AuditContext{
			Caller:          "atteler.incident",
			IssueID:         incident.RedactIdentifier(inc.Reference),
			IssueIdentifier: incident.RedactIdentifier(inc.Source),
		},
	})
	if err != nil {
		return incident.RedactCommandResult(incident.CommandResult{
			StartedAt: started,
			Duration:  time.Since(started),
			Command:   command,
			Status:    incidentCommandStatusFailed,
			Error:     err.Error(),
		})
	}

	runErr := cmd.Run()
	stdoutText := incident.RedactText(stdout.String())
	stderrText := incident.RedactText(stderr.String())
	finishErr := invocation.Finish(attshell.FinishOptions{
		Stdout:        stdoutText,
		Stderr:        stderrText,
		Error:         runErr,
		OutputCapture: attshell.OutputCaptured,
		OutputNote:    incidentOutputCaptureNote(outputLimit),
	})

	status := incidentCommandStatusPassed
	errText := ""
	if err := incidentLocalCommandError(runCtx, timeout, runErr, finishErr); err != nil {
		status = incidentCommandStatusFailed
		errText = err.Error()
	} else if outputLimit != nil && outputLimit.truncatedOutput() {
		errText = fmt.Sprintf("shell: bash command output truncated to %d bytes", defaultIncidentOutputBytes)
	}

	return incident.RedactCommandResult(incident.CommandResult{
		StartedAt: started,
		Duration:  time.Since(started),
		Command:   command,
		Status:    status,
		Stdout:    stdoutText,
		Stderr:    stderrText,
		Error:     errText,
	})
}

func incidentLocalCommandError(
	ctx context.Context,
	timeout time.Duration,
	runErr error,
	finishErr error,
) error {
	switch {
	case ctx.Err() != nil:
		return fmt.Errorf("shell: bash command timed out after %s: %w", timeout, ctx.Err())
	case runErr != nil:
		return fmt.Errorf("shell: bash command failed: %w", runErr)
	default:
		return finishErr
	}
}

func incidentOutputCaptureNote(outputLimit *incidentCommandOutputLimiter) string {
	if outputLimit != nil && outputLimit.truncatedOutput() {
		return fmt.Sprintf("incident command output truncated to %d bytes before redacted audit capture", defaultIncidentOutputBytes)
	}

	return "incident command output redacted before audit capture"
}

type incidentCommandOutputLimiter struct {
	mu        sync.Mutex
	remaining int64
	truncated bool
}

type incidentCommandOutputBuffer struct {
	limiter *incidentCommandOutputLimiter
	buffer  bytes.Buffer
}

func incidentCommandOutputBuffers(maxBytes int64) (
	stdout *incidentCommandOutputBuffer,
	stderr *incidentCommandOutputBuffer,
	limiter *incidentCommandOutputLimiter,
) {
	limiter = &incidentCommandOutputLimiter{remaining: maxBytes}

	return &incidentCommandOutputBuffer{limiter: limiter}, &incidentCommandOutputBuffer{limiter: limiter}, limiter
}

func (b *incidentCommandOutputBuffer) Write(p []byte) (int, error) {
	if b == nil || b.limiter == nil {
		return len(p), nil
	}

	b.limiter.mu.Lock()
	defer b.limiter.mu.Unlock()

	if b.limiter.remaining <= 0 {
		if len(p) > 0 {
			b.limiter.truncated = true
		}

		return len(p), nil
	}

	writeBytes := min(int64(len(p)), b.limiter.remaining)
	if writeBytes > 0 {
		_, _ = b.buffer.Write(p[:writeBytes])
		b.limiter.remaining -= writeBytes
	}
	if writeBytes < int64(len(p)) {
		b.limiter.truncated = true
	}

	return len(p), nil
}

func (b *incidentCommandOutputBuffer) String() string {
	if b == nil {
		return ""
	}

	return b.buffer.String()
}

func (l *incidentCommandOutputLimiter) truncatedOutput() bool {
	if l == nil {
		return false
	}

	l.mu.Lock()
	defer l.mu.Unlock()

	return l.truncated
}

func writeIncidentArtifacts(cwd string, analysis incident.Analysis, input incidentDiagnoseCommandInput) ([]string, error) {
	var warnings []string
	if strings.TrimSpace(input.ReportPath) != "" {
		if err := writeIncidentFile(cwd, input.ReportPath, incident.RenderMarkdown(analysis)); err != nil {
			return nil, err
		}
		warnings = appendAttelerArtifactPrivacyWarning(warnings, input.ReportPath)
	}
	if strings.TrimSpace(input.PRBodyPath) != "" {
		if err := writeIncidentFile(cwd, input.PRBodyPath, incident.RenderMarkdown(analysis)); err != nil {
			return nil, err
		}
		warnings = appendAttelerArtifactPrivacyWarning(warnings, input.PRBodyPath)
	}

	return warnings, nil
}

func appendAttelerArtifactPrivacyWarning(warnings []string, path string) []string {
	if hint, ok := attelerArtifactPrivacyHint(path); ok {
		return append(warnings, hint)
	}

	return warnings
}

func writeIncidentFile(cwd, path, content string) error {
	path, err := resolveIncidentArtifactPath(cwd, path)
	if err != nil {
		return err
	}

	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return fmt.Errorf("incident artifact: create dir %s: %s", incident.RedactIdentifier(filepath.Dir(path)), redactedIncidentError(err))
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		return fmt.Errorf("incident artifact: write %s: %s", incident.RedactIdentifier(path), redactedIncidentError(err))
	}

	return nil
}

func resolveIncidentArtifactPath(cwd, path string) (string, error) {
	resolved := resolveWorkspacePath(cwd, strings.TrimSpace(path))
	absCWD, cwdErr := filepath.Abs(cwd)
	if cwdErr != nil {
		return "", fmt.Errorf("incident artifact: resolve workspace: %s", redactedIncidentError(cwdErr))
	}
	absPath, pathErr := filepath.Abs(resolved)
	if pathErr != nil {
		return "", fmt.Errorf("incident artifact: resolve path %s: %s", incident.RedactIdentifier(path), redactedIncidentError(pathErr))
	}

	absCWD = filepath.Clean(absCWD)
	absPath = filepath.Clean(absPath)
	if absPath != absCWD && !strings.HasPrefix(absPath, absCWD+string(os.PathSeparator)) {
		return "", fmt.Errorf("incident artifact: path %s is outside workspace %s", incident.RedactIdentifier(absPath), incident.RedactIdentifier(absCWD))
	}
	if err := rejectIncidentArtifactSymlinks(absCWD, absPath); err != nil {
		return "", err
	}

	return absPath, nil
}

func rejectIncidentArtifactSymlinks(absCWD, absPath string) error {
	rel, err := filepath.Rel(absCWD, absPath)
	if err != nil {
		return fmt.Errorf("incident artifact: resolve relative path %s: %s", incident.RedactIdentifier(absPath), redactedIncidentError(err))
	}

	current := absCWD
	for part := range strings.SplitSeq(rel, string(os.PathSeparator)) {
		if part == "" || part == "." {
			continue
		}

		current = filepath.Join(current, part)
		info, statErr := os.Lstat(current)
		if errors.Is(statErr, os.ErrNotExist) {
			return nil
		}
		if statErr != nil {
			return fmt.Errorf("incident artifact: inspect path %s: %s", incident.RedactIdentifier(current), redactedIncidentError(statErr))
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("incident artifact: path %s must not traverse symlink %s", incident.RedactIdentifier(absPath), incident.RedactIdentifier(current))
		}
	}

	return nil
}

func redactedIncidentError(err error) string {
	if err == nil {
		return ""
	}

	return incident.RedactText(err.Error())
}

func redactedIncidentTextWithSensitiveValues(text string, sensitiveValues []string) string {
	for _, value := range sensitiveValues {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}

		text = strings.ReplaceAll(text, value, "[REDACTED:incident_value]")
	}

	return incident.RedactText(text)
}

func openIncidentPR(ctx context.Context, cwd string, analysis incident.Analysis, input incidentDiagnoseCommandInput, timeout time.Duration) (string, error) {
	if len(analysis.Validation) == 0 {
		return "", errors.New("incident open pr: validation was not captured; provide --incident-validation-command before opening a PR or use --incident-pr-body to write a report template")
	}
	if failed := incidentFailedValidationCommands(analysis.Validation); len(failed) > 0 {
		return "", fmt.Errorf("incident open pr: validation failed for %s; fix the failure before opening a PR or use --incident-pr-body to write a report template", strings.Join(failed, ", "))
	}

	bodyPath := strings.TrimSpace(input.PRBodyPath)
	if bodyPath == "" {
		bodyPath = filepath.Join(".atteler", "incident-pr-"+safeIncidentFilename(incident.RedactIdentifier(analysis.Incident.Reference))+".md")
		if err := writeIncidentFile(cwd, bodyPath, incident.RenderMarkdown(analysis)); err != nil {
			return "", err
		}
	}

	bodyPath, pathErr := resolveIncidentArtifactPath(cwd, bodyPath)
	if pathErr != nil {
		return "", pathErr
	}

	var stdout, stderr bytes.Buffer
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd, invocation, err := attshell.CommandContext(runCtx, attshell.CommandOptions{
		Program: "gh",
		Args: []string{
			"pr",
			"create",
			"--title",
			incident.RedactText(analysis.PRPlan.Title),
			"--body-file",
			bodyPath,
		},
		Dir:    cwd,
		Stdout: &stdout,
		Stderr: &stderr,
		Mode:   attshell.ModeCaptured,
		Policy: &attshell.Policy{
			AllowCredentialEnv: defaultIncidentGitHubTokenEnvNames,
		},
		SecretValues: incident.SensitiveValues(strings.Join([]string{
			analysis.Incident.Reference,
			analysis.Incident.Source,
			analysis.PRPlan.Title,
			bodyPath,
		}, "\n")),
		Audit: attshell.AuditContext{
			Caller:          "atteler.incident.open_pr",
			IssueID:         incident.RedactIdentifier(analysis.Incident.Reference),
			IssueIdentifier: incident.RedactIdentifier(analysis.Incident.Source),
		},
	})
	if err != nil {
		return "", fmt.Errorf("incident open pr: %w", err)
	}

	runErr := cmd.Run()
	stdoutText := incident.RedactText(stdout.String())
	stderrText := incident.RedactText(stderr.String())
	finishErr := invocation.Finish(attshell.FinishOptions{
		Stdout:        stdoutText,
		Stderr:        stderrText,
		Error:         runErr,
		OutputCapture: attshell.OutputCaptured,
		OutputNote:    "incident PR command output redacted before audit capture",
	})
	if runCtx.Err() != nil {
		return "", fmt.Errorf("incident open pr: timed out after %s: %w", timeout, runCtx.Err())
	}
	if finishErr != nil {
		return "", fmt.Errorf("incident open pr: audit: %w", finishErr)
	}
	if runErr != nil {
		return "", fmt.Errorf("incident open pr: %w: %s", runErr, stderrText)
	}

	return incidentCreatedPRURL(stdoutText, stderrText), nil
}

var incidentCreatedPRURLPattern = regexp.MustCompile(`https?://\S+/pull/\d+\S*`)

func incidentCreatedPRURL(outputs ...string) string {
	for _, output := range outputs {
		if match := incidentCreatedPRURLPattern.FindString(output); match != "" {
			match = strings.TrimRight(match, ".,;)")
			return incident.RedactIdentifier(match)
		}
	}

	return ""
}

func incidentFailedValidationCommands(results []incident.CommandResult) []string {
	failed := make([]string, 0)
	for i := range results {
		if strings.EqualFold(strings.TrimSpace(results[i].Status), incidentCommandStatusPassed) {
			continue
		}

		command := incident.RedactText(results[i].Command)
		if strings.TrimSpace(command) == "" {
			command = "validation command"
		}
		failed = append(failed, command)
	}

	return failed
}

func resolveWorkspacePath(cwd, path string) string {
	if filepath.IsAbs(path) {
		return path
	}

	return filepath.Join(cwd, path)
}

func incidentTimeout(seconds int) time.Duration {
	if seconds <= 0 {
		return defaultIncidentTimeout
	}

	return time.Duration(seconds) * time.Second
}

func normalizeIncidentOutputFormat(format string) (string, error) {
	format = strings.ToLower(strings.TrimSpace(format))
	if format == "" {
		return outputFormatText, nil
	}

	switch format {
	case outputFormatText, outputFormatJSON, commandOutputMarkdown:
		return format, nil
	default:
		return "", fmt.Errorf("unsupported incident output format %q (supported: %s, %s, %s)", format, outputFormatText, outputFormatJSON, commandOutputMarkdown)
	}
}

var safeIncidentFilenamePattern = regexp.MustCompile(`[^a-zA-Z0-9._-]+`)

func safeIncidentFilename(reference string) string {
	reference = strings.TrimSpace(reference)
	if reference == "" {
		reference = incidentUnknownReference
	}

	filename := strings.Trim(safeIncidentFilenamePattern.ReplaceAllString(reference, "-"), "-")
	if filename == "" {
		return incidentUnknownReference
	}
	if len(filename) > maxIncidentFilenameLength {
		filename = strings.Trim(filename[:maxIncidentFilenameLength], ".-_")
		if filename == "" {
			return incidentUnknownReference
		}
	}

	return filename
}
