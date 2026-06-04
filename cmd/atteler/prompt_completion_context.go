package main

import (
	"bytes"
	"context"
	"fmt"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/tommoulard/atteler/pkg/agent"
	"github.com/tommoulard/atteler/pkg/autonomy"
	"github.com/tommoulard/atteler/pkg/promptcomplete"
	"github.com/tommoulard/atteler/pkg/session"
	"github.com/tommoulard/atteler/pkg/shell"
)

const (
	maxPromptContextCandidates = 300
	maxPromptGitOutputBytes    = 64 * 1024
)

var issueRefPattern = regexp.MustCompile(`(?i)(?:\bGH-\d+\b|#\d+\b)`)

func promptCompletionContext(ctx context.Context, state appState, input string, includeRepo bool) promptcomplete.Context {
	ctx = contextWithPromptGitAutonomy(ctx, state.autonomy)

	return promptCompletionContextWithFreshness(ctx, state, input, includeRepo).Context
}

func promptGitCompletionAllowed(level autonomy.Level) bool {
	// Git completion probes are read-only, but they still pass through the
	// shell/audit layer. Low autonomy is advisory-only and must not create
	// audit files while the user is only typing a prompt.
	return autonomy.Normalize(level).Allows(autonomy.ActionFileWrite)
}

type promptGitAutonomyContextKey struct{}

func contextWithPromptGitAutonomy(ctx context.Context, level autonomy.Level) context.Context {
	if ctx == nil {
		return nil
	}

	return context.WithValue(ctx, promptGitAutonomyContextKey{}, autonomy.Normalize(level))
}

func promptGitAutonomyFromContext(ctx context.Context) autonomy.Level {
	if ctx == nil {
		return autonomy.DefaultLevel
	}

	if level, ok := ctx.Value(promptGitAutonomyContextKey{}).(autonomy.Level); ok {
		return autonomy.Normalize(level)
	}

	return autonomy.DefaultLevel
}

func dedupePromptCandidates(candidates []promptcomplete.Candidate) []promptcomplete.Candidate {
	if len(candidates) < 2 {
		return candidates
	}

	seen := make(map[string]struct{}, len(candidates))

	out := make([]promptcomplete.Candidate, 0, len(candidates))
	for _, candidate := range candidates {
		key := candidate.Kind + "\x00" + candidate.Text
		if _, ok := seen[key]; ok {
			continue
		}

		seen[key] = struct{}{}

		out = append(out, candidate)
	}

	return out
}

func promptSlashCommandCandidates() []promptcomplete.Candidate {
	descriptors := slashCommandDescriptors()

	out := make([]promptcomplete.Candidate, 0, len(descriptors))
	for i := range descriptors {
		command := &descriptors[i]
		out = append(out, promptcomplete.Candidate{
			Text:        "/" + command.Name,
			Kind:        "slash-command",
			Source:      "interactive slash commands",
			Description: command.Summary,
			Tokens:      append([]string(nil), command.CompletionTokens...),
		})

		for _, alias := range command.Aliases {
			out = append(out, promptcomplete.Candidate{
				Text:        "/" + alias,
				Kind:        "slash-command",
				Source:      "interactive slash commands",
				Description: command.Summary,
				Tokens:      append([]string(nil), command.CompletionTokens...),
			})
		}
	}

	return out
}

func promptCompletionPrefix(input string) string {
	cursor := len(input)
	start := cursor

	for start > 0 && isPromptCompletionTokenByte(input[start-1]) {
		start--
	}

	return input[start:cursor]
}

func isPromptCompletionTokenByte(b byte) bool {
	return (b >= 'A' && b <= 'Z') ||
		(b >= 'a' && b <= 'z') ||
		(b >= '0' && b <= '9') ||
		strings.ContainsRune("_@$/.\\-:#", rune(b))
}

func symbolNameMatchesPrefix(name, prefix string) bool {
	name = strings.ToLower(name)

	return strings.HasPrefix(name, prefix) || (len(prefix) >= 3 && strings.Contains(name, prefix))
}

func promptProjectSymbolCandidates(ctx context.Context, root, input string) []promptcomplete.Candidate {
	candidates, err := promptProjectSymbolCandidatesWithError(
		ctx,
		root,
		input,
		maxPromptContextCandidates,
		defaultPromptContextMaxIndexFiles,
	)
	if err != nil {
		return nil
	}

	return candidates
}

func promptGitRecentFileCandidates(ctx context.Context, root string) []promptcomplete.Candidate {
	candidates, err := promptGitRecentFileCandidatesWithError(ctx, root, maxPromptContextCandidates)
	if err != nil {
		return nil
	}

	return candidates
}

func promptGitIssueCandidates(ctx context.Context, root string) []promptcomplete.Candidate {
	candidates, err := promptGitIssueCandidatesWithError(ctx, root, maxPromptContextCandidates)
	if err != nil {
		return nil
	}

	return candidates
}

func promptGitOutput(ctx context.Context, root string, args ...string) (string, error) {
	stdout := newPromptLimitedOutputBuffer(maxPromptGitOutputBytes)
	stderr := newPromptLimitedOutputBuffer(maxPromptGitOutputBytes)

	cmd, invocation, err := shell.CommandContext(ctx, shell.CommandOptions{
		Program: "git",
		Args:    args,
		Dir:     root,
		Stdout:  stdout,
		Stderr:  stderr,
		Mode:    shell.ModeCaptured,
		Audit:   promptGitAuditContext(promptGitAutonomyFromContext(ctx)),
	})
	if err != nil {
		return "", fmt.Errorf("prompt git: authorize: %w", err)
	}

	runErr := cmd.Run()

	stdoutText := stdout.String()
	stderrText := stderr.String()
	outputNote := ""

	if stdout.Truncated() || stderr.Truncated() {
		outputNote = fmt.Sprintf("prompt git output limited to %d bytes per stream", maxPromptGitOutputBytes)
	}

	if finishErr := invocation.Finish(shell.FinishOptions{
		Stdout:        stdoutText,
		Stderr:        stderrText,
		Error:         runErr,
		OutputCapture: shell.OutputCaptured,
		OutputNote:    outputNote,
	}); finishErr != nil {
		return "", fmt.Errorf("prompt git: audit: %w", finishErr)
	}

	if runErr != nil {
		return "", fmt.Errorf("prompt git: run: %w", runErr)
	}

	if stdout.Truncated() || stderr.Truncated() {
		return stdoutText, promptContextPartialError{
			reason:          fmt.Sprintf("git output exceeded %d bytes per stream", maxPromptGitOutputBytes),
			outputTruncated: stdout.Truncated(),
		}
	}

	return stdoutText, nil
}

type promptLimitedOutputBuffer struct {
	buffer    bytes.Buffer
	maxBytes  int
	truncated bool
}

func newPromptLimitedOutputBuffer(maxBytes int) *promptLimitedOutputBuffer {
	return &promptLimitedOutputBuffer{maxBytes: maxBytes}
}

func (b *promptLimitedOutputBuffer) Write(p []byte) (int, error) {
	if b == nil {
		return len(p), nil
	}

	if b.maxBytes <= 0 {
		_, _ = b.buffer.Write(p)

		return len(p), nil
	}

	remaining := b.maxBytes - b.buffer.Len()
	if remaining <= 0 {
		b.truncated = true

		return len(p), nil
	}

	if len(p) > remaining {
		_, _ = b.buffer.Write(p[:remaining])
		b.truncated = true

		return len(p), nil
	}

	_, _ = b.buffer.Write(p)

	return len(p), nil
}

func (b *promptLimitedOutputBuffer) String() string {
	if b == nil {
		return ""
	}

	return b.buffer.String()
}

func (b *promptLimitedOutputBuffer) Truncated() bool {
	return b != nil && b.truncated
}

func promptGitAuditContext(level autonomy.Level) shell.AuditContext {
	return shell.AuditContext{
		Caller:   "atteler.prompt_completion.git",
		Autonomy: autonomy.Normalize(level).String(),
	}
}

func parseGitStatusPath(line string) string {
	line = strings.TrimSpace(line)
	if len(line) < 3 {
		return ""
	}

	path := strings.TrimSpace(line[2:])
	if path == "" {
		return ""
	}

	if _, after, ok := strings.Cut(path, " -> "); ok {
		path = strings.TrimSpace(after)
	}

	return path
}

func promptSessionFileCandidates(sessionState session.Session, limits ...int) []promptcomplete.Candidate {
	if len(sessionState.Artifacts) == 0 {
		return nil
	}

	limit := promptContextCandidateLimit(limits...)
	if limit <= 0 {
		limit = maxPromptContextCandidates
	}

	artifacts := tailPromptSessionArtifacts(sessionState.Artifacts, limit)

	out := make([]promptcomplete.Candidate, 0, min(len(artifacts), limit))
	for i := range artifacts {
		if len(out) >= limit {
			break
		}

		artifact := &artifacts[i]

		path := strings.TrimSpace(artifact.Path)
		if path == "" {
			continue
		}

		out = append(out, promptcomplete.Candidate{
			Text:        filepath.ToSlash(path),
			Kind:        "recent-file",
			Source:      "session artifacts",
			Description: artifactDescription(artifact),
			Tokens:      []string{artifact.Kind, artifact.SourceAgent, filepath.Base(path)},
		})
	}

	return out
}

func artifactDescription(artifact *session.Artifact) string {
	parts := []string{"session artifact"}
	if artifact.Kind != "" {
		parts = append(parts, "kind="+artifact.Kind)
	}

	if artifact.Summary != "" {
		parts = append(parts, artifact.Summary)
	}

	return strings.Join(parts, "; ")
}

func promptIssueCandidatesFromSession(sessionState session.Session, limits ...int) []promptcomplete.Candidate {
	limit := promptContextCandidateLimit(limits...)
	if limit <= 0 {
		limit = maxPromptContextCandidates
	}

	texts := []string{sessionState.ID, promptContextStringSample(sessionState.Title)}
	for _, tag := range tailPromptSessionStrings(sessionState.Tags, limit) {
		texts = append(texts, promptContextStringSample(tag))
	}

	for _, message := range tailPromptSessionMessages(sessionState.Messages, limit) {
		texts = append(texts, promptContextStringSample(message.Content))
	}

	artifacts := tailPromptSessionArtifacts(sessionState.Artifacts, limit)
	for i := range artifacts {
		artifact := &artifacts[i]
		texts = append(texts, artifact.Path, artifact.Kind, promptContextStringSample(artifact.Summary))
	}

	return issueCandidatesFromTextLimit("session state", limit, texts...)
}

func promptIssueCandidatesFromTasks(tasks []promptcomplete.Candidate) []promptcomplete.Candidate {
	texts := make([]string, 0, len(tasks)*2)
	for i := range tasks {
		task := &tasks[i]
		texts = append(texts, task.Text, task.Description)
	}

	return issueCandidatesFromText("task list", texts...)
}

func issueCandidatesFromText(source string, texts ...string) []promptcomplete.Candidate {
	return issueCandidatesFromTextLimit(source, maxPromptContextCandidates, texts...)
}

func issueCandidatesFromTextLimit(source string, limit int, texts ...string) []promptcomplete.Candidate {
	if limit <= 0 {
		limit = maxPromptContextCandidates
	}

	seen := make(map[string]struct{})
	refs := make([]string, 0)

	for _, text := range texts {
		for _, ref := range issueRefPattern.FindAllString(text, -1) {
			ref = normalizeIssueRef(ref)
			if ref == "" {
				continue
			}

			if _, ok := seen[ref]; ok {
				continue
			}

			seen[ref] = struct{}{}

			refs = append(refs, ref)
			if len(refs) >= limit {
				break
			}
		}

		if len(refs) >= limit {
			break
		}
	}

	sort.Strings(refs)

	out := make([]promptcomplete.Candidate, 0, len(refs))
	for _, ref := range refs {
		out = append(out, promptcomplete.Candidate{
			Text:        ref,
			Kind:        "issue",
			Source:      source,
			Description: "issue reference from " + source,
			Tokens:      []string{"issue", "github"},
		})
	}

	return out
}

func normalizeIssueRef(ref string) string {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return ""
	}

	if strings.HasPrefix(ref, "#") {
		return ref
	}

	return strings.ToUpper(ref[:2]) + ref[2:]
}

func promptPermissionCandidates(registry *agent.Registry, selectedAgent string) []promptcomplete.Candidate {
	out := []promptcomplete.Candidate{{
		Text:        "local-only",
		Kind:        "permission",
		Source:      "completion modes",
		Description: "use deterministic no-network completion context",
		Tokens:      []string{"local", "no-network", "offline"},
	}}

	if strings.TrimSpace(selectedAgent) == "" || registry == nil {
		return out
	}

	activeAgent, ok := registry.Get(selectedAgent)
	if !ok {
		return out
	}

	if activeAgent.Hidden {
		return out
	}

	permissions := activeAgent.ToolPermissions
	if permissions == nil {
		out = append(out, promptcomplete.Candidate{
			Text:        "bash",
			Kind:        "permission",
			Source:      "agent permissions",
			Description: selectedAgent + " uses the default tool policy; bash is available with safety checks",
			Tokens:      []string{"tool", "allowed", selectedAgent},
		})

		return out
	}

	names := make([]string, 0, len(permissions))
	for name, allowed := range permissions {
		if allowed {
			names = append(names, name)
		}
	}

	sort.Strings(names)

	for _, name := range names {
		out = append(out, promptcomplete.Candidate{
			Text:        name,
			Kind:        "permission",
			Source:      "agent permissions",
			Description: selectedAgent + " is allowed to use tool " + name,
			Tokens:      []string{"tool", "allowed", selectedAgent},
		})
	}

	return out
}

func relPath(root, path string) string {
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return path
	}

	return rel
}
