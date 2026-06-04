package main

import (
	"flag"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestTranslateCLIArgs_DomainCommandsMapToCompatibilityFlags(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		args []string
		want []string
	}{
		{
			name: "code intel command",
			args: []string{"code-intel", "summary"},
			want: []string{"--code-summary"},
		},
		{
			name: "code intel command keeps json flag",
			args: []string{"code-intel", "summary", "--json"},
			want: []string{"--code-summary", "--json"},
		},
		{
			name: "code intel command keeps domain scoped json flag",
			args: []string{"code-intel", "--json", "summary"},
			want: []string{"--json", "--code-summary"},
		},
		{
			name: "code intel command keeps leading json flag",
			args: []string{"--json", "code-intel", "summary"},
			want: []string{"--json", "--code-summary"},
		},
		{
			name: "code intel command keeps output format flag",
			args: []string{"code-intel", "summary", "--output", "json"},
			want: []string{"--code-summary", "--output", "json"},
		},
		{
			name: "code intel lsp workspace keeps json flag",
			args: []string{"code-intel", "lsp-workspace", "Handler", "--json"},
			want: []string{"--lsp-workspace-symbols", "Handler", "--json"},
		},
		{
			name: "code intel routes descriptor generated command",
			args: []string{"code-intel", "package-import-path", "main:context"},
			want: []string{"--code-package-import-path", "main:context"},
		},
		{
			name: "review command keeps follow-on flags",
			args: []string{"review", "plan", "--review-agent", "quality-reviewer"},
			want: []string{"--review-plan", "--review-agent", "quality-reviewer"},
		},
		{
			name: "plugin string command",
			args: []string{"plugins", "run", "reviewer/check", "--plugin-dry-run"},
			want: []string{"--run-plugin", "reviewer/check", "--plugin-dry-run"},
		},
		{
			name: "worktrees run auto merge gate",
			args: []string{"worktrees", "run", "add", "tests", "--worktree-auto-merge", "--worktree-verify-command", "go test ./..."},
			want: []string{"--worktree", "--worktree-auto-merge", "--worktree-verify-command", "go test ./...", "add tests"},
		},
		{
			name: "leading flags stay parseable before domain command",
			args: []string{"--model", "openai/gpt-5.4", "chat", "once", "explain", "this", "repo"},
			want: []string{"--model", "openai/gpt-5.4", "--once", "explain this repo"},
		},
		{
			name: "joined prompt command keeps trailing flags parseable",
			args: []string{"chat", "once", "explain", "this", "repo", "--model", "openai/gpt-5.4"},
			want: []string{"--once", "explain this repo", "--model", "openai/gpt-5.4"},
		},
		{
			name: "joined prompt command keeps trailing autonomy flag parseable",
			args: []string{"chat", "once", "implement", "GH-123", "--autonomy", "high"},
			want: []string{"--once", "implement GH-123", "--autonomy", "high"},
		},
		{
			name: "memory search keeps trailing privacy flags parseable",
			args: []string{"memory", "search", "OAuth", "retry", "--memory-scope", "repo", "--memory-tag", "security"},
			want: []string{"--memory-search", "OAuth retry", "--memory-scope", "repo", "--memory-tag", "security"},
		},
		{
			name: "incident diagnose maps to incident flags",
			args: []string{"incident", "diagnose", "--sentry", "ISSUE-912"},
			want: []string{"--incident-diagnose", "--sentry", "ISSUE-912"},
		},
		{
			name: "incident diagnose keeps json flag",
			args: []string{"incident", "diagnose", "--incident-file", "incident.json", "--json"},
			want: []string{"--incident-diagnose", "--incident-file", "incident.json", "--json"},
		},
		{
			name: "positional run command keeps trailing flags parseable",
			args: []string{"chat", "run", "explain", "this", "repo", "--model", "openai/gpt-5.4"},
			want: []string{"--model", "openai/gpt-5.4", "explain this repo"},
		},
		{
			name: "positional run command preserves dash-prefixed prompt after delimiter",
			args: []string{"chat", "run", "--", "--not-a-flag", "prompt"},
			want: []string{"--", "--not-a-flag prompt"},
		},
		{
			name: "domain scoped legacy flag",
			args: []string{"code", "--code-summary"},
			want: []string{"--code-summary"},
		},
		{
			name: "unknown first word remains positional prompt",
			args: []string{"explain", "this", "repo"},
			want: []string{"explain", "this", "repo"},
		},
		{
			name: "flag delimiter keeps following domain-like words positional",
			args: []string{"--", "code-intel", "summary"},
			want: []string{"--", "code-intel", "summary"},
		},
		{
			name: "joined prompt command",
			args: []string{"chat", "once", "explain", "this", "repo"},
			want: []string{"--once", "explain this repo"},
		},
		{
			name: "stdin-only one-shot command",
			args: []string{"chat", "once", "--stdin"},
			want: []string{"--stdin"},
		},
		{
			name: "issue implement command",
			args: []string{"issue", "implement", "GH-218", "--open-pr", "--base", "main", "--run-tests", "--run-lint"},
			want: []string{"--issue-implement", "GH-218", "--open-pr", "--base", "main", "--run-tests", "--run-lint"},
		},
		{
			name: "issue implement alias",
			args: []string{"issues", "implement", "#218", "--open-pr"},
			want: []string{"--issue-implement", "#218", "--open-pr"},
		},
		{
			name: "issue implement url reference",
			args: []string{"issue", "implement", "https://github.com/owner/repo/issues/218", "--open-pr"},
			want: []string{"--issue-implement", "https://github.com/owner/repo/issues/218", "--open-pr"},
		},
		{
			name: "issue implement keeps flags before issue reference",
			args: []string{"issue", "implement", "--open-pr", "--base", "main", "GH-218"},
			want: []string{"--issue-implement", "GH-218", "--open-pr", "--base", "main"},
		},
		{
			name: "issue implement keeps domain scoped flags before command",
			args: []string{"issue", "--open-pr", "--base", "main", "implement", "GH-218"},
			want: []string{"--open-pr", "--base", "main", "--issue-implement", "GH-218"},
		},
		{
			name: "issue implement docs changelog and workflow flags",
			args: []string{"issue", "implement", "GH-218", "--issue-workflow", "custom/WORKFLOW.md", "--update-docs", "--update-changelog"},
			want: []string{"--issue-implement", "GH-218", "--issue-workflow", "custom/WORKFLOW.md", "--update-docs", "--update-changelog"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := translateCLIArgsWithFlagSet(tt.args, newRegisteredFlagSetForTest(t))
			require.NoError(t, got.Err)
			assert.False(t, got.Help)
			assert.Equal(t, tt.want, got.Args)
		})
	}
}

func TestTranslateCLIArgs_CodeIntelDescriptorCommandsRouteToLegacyFlags(t *testing.T) {
	t.Parallel()

	fs := newRegisteredFlagSetForTest(t)

	for _, descriptor := range codeIntelCommandDescriptors() {
		t.Run(descriptor.DomainCommand, func(t *testing.T) {
			t.Parallel()

			args := []string{"code-intel", descriptor.DomainCommand}
			want := []string{descriptor.LegacyFlag}

			if strings.Contains(descriptor.Args, "<") {
				args = append(args, "fixture")
				want = append(want, "fixture")
			}

			got := translateCLIArgsWithFlagSet(args, fs)
			require.NoError(t, got.Err)
			assert.False(t, got.Help)
			assert.Equal(t, want, got.Args)

			jsonArgs := append(append([]string(nil), args...), "--json")
			jsonWant := append(append([]string(nil), want...), "--json")
			jsonGot := translateCLIArgsWithFlagSet(jsonArgs, fs)
			require.NoError(t, jsonGot.Err)
			assert.False(t, jsonGot.Help)
			assert.Equal(t, jsonWant, jsonGot.Args)

			outputJSONArgs := append(append([]string(nil), args...), "--output", "json")
			outputJSONWant := append(append([]string(nil), want...), "--output", "json")
			outputJSONGot := translateCLIArgsWithFlagSet(outputJSONArgs, fs)
			require.NoError(t, outputJSONGot.Err)
			assert.False(t, outputJSONGot.Help)
			assert.Equal(t, outputJSONWant, outputJSONGot.Args)
		})
	}
}

func TestTranslateCLIArgs_CodeIntelPaginationFlagsStayWithGroupedCommand(t *testing.T) {
	t.Parallel()

	got := translateCLIArgsWithFlagSet(
		[]string{"code-intel", "symbol", "main", "--code-limit", "1", "--code-offset", "2"},
		newRegisteredFlagSetForTest(t),
	)
	require.NoError(t, got.Err)
	assert.False(t, got.Help)
	assert.Equal(t, []string{"--code-symbol", "main", "--code-limit", "1", "--code-offset", "2"}, got.Args)
}

func TestTranslateCLIArgs_CodeIntelQueryKeepsLanguageFilter(t *testing.T) {
	t.Parallel()

	got := translateCLIArgsWithFlagSet(
		[]string{"code-intel", "query", "definitions:helper", "--code-language", "python"},
		newRegisteredFlagSetForTest(t),
	)
	require.NoError(t, got.Err)
	assert.False(t, got.Help)
	assert.Equal(t, []string{"--code-query", "definitions:helper", "--code-language", "python"}, got.Args)
}

func TestTranslateCLIArgs_DomainWordsCanStartPositionalPrompts(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		args []string
		want []string
	}{
		{
			name: "review prompt",
			args: []string{"review", "this", "diff"},
			want: []string{"review", "this", "diff"},
		},
		{
			name: "two-token review prompt",
			args: []string{"review", "code"},
			want: []string{"review", "code"},
		},
		{
			name: "watch prompt",
			args: []string{"watch", "the", "logs"},
			want: []string{"watch", "the", "logs"},
		},
		{
			name: "two-token watch prompt",
			args: []string{"watch", "logs"},
			want: []string{"watch", "logs"},
		},
		{
			name: "code alias prompt",
			args: []string{"code", "a", "parser"},
			want: []string{"code", "a", "parser"},
		},
		{
			name: "two-token code alias prompt",
			args: []string{"code", "parser"},
			want: []string{"code", "parser"},
		},
		{
			name: "leading flags stay with domain-word prompt",
			args: []string{"--model", "test/model", "config", "yaml", "defaults"},
			want: []string{"--model", "test/model", "config", "yaml", "defaults"},
		},
		{
			name: "dash-prefixed prompt token after domain word",
			args: []string{"review", "--strictly", "for", "correctness"},
			want: []string{"review", "--strictly", "for", "correctness"},
		},
		{
			name: "dash-prefixed prompt token with equals after domain word",
			args: []string{"code", "--style=functional", "parser"},
			want: []string{"code", "--style=functional", "parser"},
		},
		{
			name: "unknown dash-prefixed prompt token keeps trailing help data",
			args: []string{"review", "--strictly", "--help"},
			want: []string{"review", "--strictly", "--help"},
		},
		{
			name: "known flag before unknown command keeps domain prompt",
			args: []string{"review", "--model", "test/model", "code"},
			want: []string{"review", "--model", "test/model", "code"},
		},
		{
			name: "help me prompt",
			args: []string{"help", "me"},
			want: []string{"help", "me"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := translateCLIArgs(tt.args)
			require.NoError(t, got.Err)
			assert.False(t, got.Help)
			assert.Equal(t, tt.want, got.Args)
		})
	}
}

func TestCLIFlags_ParseSpawnExecutionBudgets(t *testing.T) {
	t.Parallel()

	opts, fs := newCLIOptionsAndFlagSetForTest(t)
	err := fs.Parse([]string{
		"--spawn-task-timeout-seconds", "5",
		"--spawn-max-concurrency", "2",
		"--spawn-retries", "1",
		"--spawn-retry-backoff-seconds", "3",
		"--spawn-token-budget", "100",
		"--spawn-cost-budget-micros", "200",
		"--spawn-output-budget-bytes", "300",
	})

	require.NoError(t, err)
	assert.Equal(t, 5, opts.spawnTaskTimeout.value)
	assert.Equal(t, 2, opts.spawnMaxConcurrency.value)
	assert.Equal(t, 1, opts.spawnRetries.value)
	assert.True(t, opts.spawnRetries.set)
	assert.Equal(t, 3, opts.spawnRetryBackoff.value)
	assert.True(t, opts.spawnRetryBackoff.set)
	assert.Equal(t, 100, opts.spawnTokenBudget.value)
	assert.Equal(t, 200, opts.spawnCostBudgetMicros.value)
	assert.Equal(t, 300, opts.spawnOutputBudgetBytes.value)
}

func TestTranslateCLIArgs_DomainLevelFlagsCanPrecedeCommands(t *testing.T) {
	t.Parallel()

	_, fs := newCLIOptionsAndFlagSetForTest(t)

	tests := []struct {
		name string
		args []string
		want []string
	}{
		{
			name: "session selector before session command",
			args: []string{"session", "--session", "demo", "messages"},
			want: []string{"--session", "demo", "--list-messages"},
		},
		{
			name: "session selector before run listing command",
			args: []string{"session", "--session", "demo", "runs"},
			want: []string{"--session", "demo", "--list-runs"},
		},
		{
			name: "session selector before run show command",
			args: []string{"session", "--session", "demo", "show-run", "latest"},
			want: []string{"--session", "demo", "--show-run", "latest"},
		},
		{
			name: "session selector before run export command",
			args: []string{"session", "--session", "demo", "export-run", "review", "--export-format", "json"},
			want: []string{"--session", "demo", "--export-run", "review", "--export-format", "json"},
		},
		{
			name: "session selector before run replay command",
			args: []string{"session", "--session", "demo", "replay-run", "speculation"},
			want: []string{"--session", "demo", "--replay-run", "speculation"},
		},
		{
			name: "session selector before run resume command",
			args: []string{"session", "--session", "demo", "resume-run", "latest"},
			want: []string{"--session", "demo", "--resume-run", "latest"},
		},
		{
			name: "top-level stdin before one-shot command",
			args: []string{"--stdin", "chat", "once"},
			want: []string{"--stdin"},
		},
		{
			name: "domain-level stdin before one-shot command",
			args: []string{"chat", "--stdin", "once"},
			want: []string{"--stdin"},
		},
		{
			name: "domain-level model before one-shot command",
			args: []string{"chat", "--model", "test/model", "once", "summarize", "--output", "json"},
			want: []string{"--model", "test/model", "--once", "summarize", "--output", "json"},
		},
		{
			name: "bool option before watch command",
			args: []string{"watch", "--watch-json", "scan"},
			want: []string{"--watch-json", "--watch-scan"},
		},
		{
			name: "mcp manifest before plugin tool alias",
			args: []string{"plugins", "--mcp-manifest", "mcp.yaml", "tool", "lookup"},
			want: []string{"--mcp-manifest", "mcp.yaml", "--mcp-tool", "lookup"},
		},
		{
			name: "domain-level stdin before replay fixture prompt source",
			args: []string{"eval", "--stdin", "replay-response", "fixture.json"},
			want: []string{"--stdin", "--replay-response", "fixture.json"},
		},
		{
			name: "top-level stdin before replay fixture prompt source",
			args: []string{"--stdin", "eval", "replay-response", "fixture.json"},
			want: []string{"--stdin", "--replay-response", "fixture.json"},
		},
		{
			name: "legacy flag with command-looking value remains legacy",
			args: []string{"plugins", "--run-plugin", "list"},
			want: []string{"--run-plugin", "list"},
		},
		{
			name: "scoped legacy flag without command still works",
			args: []string{"code", "--code-summary"},
			want: []string{"--code-summary"},
		},
		{
			name: "scoped legacy flag after domain alias still works",
			args: []string{"review", "--review-scan"},
			want: []string{"--review-scan"},
		},
		{
			name: "domain-level model before review command",
			args: []string{"review", "--model", "test/model", "scan"},
			want: []string{"--model", "test/model", "--review-scan"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := translateCLIArgsWithFlagSet(tt.args, fs)
			require.NoError(t, got.Err)
			assert.False(t, got.Help)
			assert.Equal(t, tt.want, got.Args)
		})
	}
}

func TestTranslateCLIArgs_AcceptanceDomainsRouteToLegacyCompatibility(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		args []string
		want []string
	}{
		{name: "chat session", args: []string{"session", "list"}, want: []string{"--list-sessions"}},
		{name: "chat session record failure", args: []string{"session", "record-failure", "tried", "cache"}, want: []string{"--record-failure", "tried cache"}},
		{name: "chat session record artifact provenance", args: []string{"session", "record-artifact", "artifact.md", "--artifact-logical-path", "docs/decision.md", "--artifact-review-status", "approved"}, want: []string{"--record-artifact", "artifact.md", "--artifact-logical-path", "docs/decision.md", "--artifact-review-status", "approved"}},
		{name: "chat session merge artifacts json", args: []string{"session", "merge-artifacts", "merged.json", "--merge-artifacts-format", "json"}, want: []string{"--merge-artifacts", "merged.json", "--merge-artifacts-format", "json"}},
		{name: "config", args: []string{"config", "validate"}, want: []string{"--validate-config"}},
		{name: "config migrate", args: []string{"config", "migrate"}, want: []string{"--config-migrate"}},
		{name: "config report", args: []string{"config", "report"}, want: []string{"--config-report"}},
		{name: "config explain", args: []string{"config", "explain", "default_model"}, want: []string{"--explain-config", "default_model"}},
		{name: "config doctor offline json", args: []string{"config", "doctor-offline", "--output", "json"}, want: []string{"--doctor-offline", "--output", "json"}},
		{name: "providers", args: []string{"providers", "list"}, want: []string{"--list-providers"}},
		{name: "agents", args: []string{"agents", "plan", "review", "auth"}, want: []string{"--plan-agents", "review auth"}},
		{name: "agents performance", args: []string{"agents", "performance"}, want: []string{"--agent-performance-summary"}},
		{name: "agents feedback apply", args: []string{"agents", "feedback-apply", "agents.yaml"}, want: []string{"--feedback-apply-config", "agents.yaml"}},
		{name: "agents feedback approve", args: []string{"agents", "--feedback-approve-agent", "reviewer", "--feedback-approve-id", "fg-1", "feedback-approve", "agents.yaml"}, want: []string{"--feedback-approve-agent", "reviewer", "--feedback-approve-id", "fg-1", "--feedback-approve-config", "agents.yaml"}},
		{name: "agents feedback rollback", args: []string{"agents", "--feedback-rollback-agent", "reviewer", "--feedback-rollback-id", "fg-1", "feedback-rollback", "agents.yaml"}, want: []string{"--feedback-rollback-agent", "reviewer", "--feedback-rollback-id", "fg-1", "--feedback-rollback-config", "agents.yaml"}},
		{name: "agents skill learning list", args: []string{"agents", "skill-learning-list"}, want: []string{"--skill-learning-list"}},
		{name: "agents skill learning show", args: []string{"agents", "skill-learning-show", "k8s-investigation"}, want: []string{"--skill-learning-show", "k8s-investigation"}},
		{name: "agents skill learning edit", args: []string{"agents", "skill-learning-edit", "k8s-investigation"}, want: []string{"--skill-learning-edit", "k8s-investigation"}},
		{name: "agents skill learning enable", args: []string{"agents", "skill-learning-enable", "k8s-investigation"}, want: []string{"--skill-learning-enable", "k8s-investigation"}},
		{name: "agents skill learning delete with custom dir", args: []string{"agents", "--skill-learning-dir", ".atteler/learning", "skill-learning-delete", "k8s-investigation"}, want: []string{"--skill-learning-dir", ".atteler/learning", "--skill-learning-delete", "k8s-investigation"}},
		{name: "agents skill learning opt out", args: []string{"agents", "skill-learning-disable-all"}, want: []string{"--skill-learning-disable-all"}},
		{name: "agents skill learning opt in", args: []string{"agents", "skill-learning-enable-all"}, want: []string{"--skill-learning-enable-all"}},
		{name: "agents bash", args: []string{"agents", "bash", "go", "test", "./cmd/atteler"}, want: []string{"--bash", "go test ./cmd/atteler"}},
		{name: "memory rag", args: []string{"memory", "search", "OAuth", "retry"}, want: []string{"--memory-search", "OAuth retry"}},
		{name: "memory index ttl", args: []string{"memory", "index", "note.txt", "--memory-ttl-seconds", "60"}, want: []string{"--memory-index", "note.txt", "--memory-ttl-seconds", "60"}},
		{name: "memory search raw transcript opt-in", args: []string{"memory", "search", "OAuth", "--memory-include-session-messages"}, want: []string{"--memory-search", "OAuth", "--memory-include-session-messages"}},
		{name: "memory purge", args: []string{"memory", "purge", "tag:auth"}, want: []string{"--memory-purge", "tag:auth"}},
		{name: "memory rebuild", args: []string{"memory", "rebuild"}, want: []string{"--memory-rebuild"}},
		{name: "memory list corpus", args: []string{"memory", "list-corpus"}, want: []string{"--memory-list-corpus"}},
		{name: "memory list corpus with scoped store", args: []string{"memory", "--memory-store", ".atteler/memory.json", "list-corpus"}, want: []string{"--memory-store", ".atteler/memory.json", "--memory-list-corpus"}},
		{name: "memory list corpus keeps trailing store flag", args: []string{"memory", "list-corpus", "--memory-store", ".atteler/memory.json"}, want: []string{"--memory-list-corpus", "--memory-store", ".atteler/memory.json"}},
		{name: "memory rebuild keeps privacy flags", args: []string{"memory", "rebuild", "--memory-store", ".atteler/memory.json", "--memory-scope", "repo"}, want: []string{"--memory-rebuild", "--memory-store", ".atteler/memory.json", "--memory-scope", "repo"}},
		{name: "memory purge keeps domain flags", args: []string{"memory", "--memory-store", ".atteler/memory.json", "purge", "session:abc"}, want: []string{"--memory-store", ".atteler/memory.json", "--memory-purge", "session:abc"}},
		{name: "memory purge keeps trailing store flag", args: []string{"memory", "purge", "session:abc", "--memory-store", ".atteler/memory.json"}, want: []string{"--memory-purge", "session:abc", "--memory-store", ".atteler/memory.json"}},
		{name: "memory vector lexical", args: []string{"memory", "vector-search", "redirect", "risks", "--vectorizer", "lexical"}, want: []string{"--vector-search", "redirect risks", "--vectorizer", "lexical"}},
		{name: "memory vector embedding index", args: []string{"memory", "vector-index", "docs/research.md", "--vectorizer", "embedding", "--vector-model", "nomic-embed-text"}, want: []string{"--vector-index", "docs/research.md", "--vectorizer", "embedding", "--vector-model", "nomic-embed-text"}},
		{name: "code intel", args: []string{"code-intel", "summary"}, want: []string{"--code-summary"}},
		{name: "review", args: []string{"review", "scan"}, want: []string{"--review-scan"}},
		{name: "watch", args: []string{"watch", "json"}, want: []string{"--watch-scan", "--watch-json"}},
		{name: "plugins", args: []string{"plugins", "describe", "reviewer"}, want: []string{"--describe-plugin", "reviewer"}},
		{name: "worktrees", args: []string{"worktrees", "merge", "session-123"}, want: []string{"--merge-worktree", "session-123"}},
		{name: "worktrees merge base mismatch override", args: []string{"worktrees", "merge", "session-123", "--merge-worktree-allow-base-mismatch"}, want: []string{"--merge-worktree", "session-123", "--merge-worktree-allow-base-mismatch"}},
		{name: "worktrees merge verification command", args: []string{"worktrees", "merge", "session-123", "--worktree-verify-command", "go test ./..."}, want: []string{"--merge-worktree", "session-123", "--worktree-verify-command", "go test ./..."}},
		{name: "worktrees merge verification override", args: []string{"worktrees", "merge", "session-123", "--worktree-merge-override"}, want: []string{"--merge-worktree", "session-123", "--worktree-merge-override"}},
		{name: "worktrees run prompt", args: []string{"worktrees", "run", "add", "tests"}, want: []string{"--worktree", "add tests"}},
		{name: "eval", args: []string{"eval", "output", "actual.txt"}, want: []string{"--eval-output", "actual.txt"}},
		{name: "eval run", args: []string{"eval", "run", "suite.eval.yaml"}, want: []string{"--eval-assertions", "suite.eval.yaml"}},
		{name: "eval fixtures", args: []string{"eval", "fixtures", ".atteler/evals"}, want: []string{"--eval-fixture-dir", ".atteler/evals"}},
		{name: "eval list", args: []string{"eval", "list"}, want: []string{"--list-evaluations"}},
		{name: "eval record response with prompt", args: []string{"eval", "record-response", "fixture.json", "summarize", "readme"}, want: []string{"--record-response", "fixture.json", "--once", "summarize readme"}},
		{name: "eval replay response with prompt and flags", args: []string{"eval", "replay-response", "fixture.json", "summarize", "readme", "--output", "json"}, want: []string{"--replay-response", "fixture.json", "--once", "summarize readme", "--output", "json"}},
		{name: "eval replay response with stdin", args: []string{"eval", "replay-response", "fixture.json", "--stdin"}, want: []string{"--replay-response", "fixture.json", "--stdin"}},
		{name: "agent memory delete", args: []string{"memory", "agent-delete", "delete-me", "--agent-memory-agent", "reviewer"}, want: []string{"--agent-memory-delete", "delete-me", "--agent-memory-agent", "reviewer"}},
		{name: "agent memory compact", args: []string{"memory", "agent-compact", "--agent-memory-store", "store.json"}, want: []string{"--agent-memory-compact", "--agent-memory-store", "store.json"}},
		{name: "agent memory migrate", args: []string{"memory", "agent-migrate", "--agent-memory-store", "store.json"}, want: []string{"--agent-memory-migrate", "--agent-memory-store", "store.json"}},
		{name: "agent memory index ttl", args: []string{"memory", "agent-index", "note.txt", "--agent-memory-ttl-seconds", "60"}, want: []string{"--agent-memory-index", "note.txt", "--agent-memory-ttl-seconds", "60"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := translateCLIArgs(tt.args)
			require.NoError(t, got.Err)
			assert.False(t, got.Help)
			assert.Equal(t, tt.want, got.Args)
		})
	}
}

func TestRegisterCLIFlags_MemoryRedactKeepsRegexCommas(t *testing.T) {
	t.Parallel()

	opts, fs := newCLIOptionsAndFlagSetForTest(t)

	require.NoError(t, fs.Parse([]string{
		"--memory-redact", `ACME-[0-9]{2,4}`,
		"--memory-redact", `token=(foo,bar)`,
	}))
	assert.Equal(t, rawStringListFlag{`ACME-[0-9]{2,4}`, `token=(foo,bar)`}, opts.memoryRedactRules)
}

func TestTranslateCLIArgs_CanonicalDomainsAndAliasesRoute(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		args []string
		want []string
	}{
		{name: "canonical slash chat session", args: []string{"chat/session", "once", "hello"}, want: []string{"--once", "hello"}},
		{name: "canonical slash memory retrieval", args: []string{"memory/retrieval", "search", "token", "refresh"}, want: []string{"--memory-search", "token refresh"}},
		{name: "legacy memory rag alias", args: []string{"rag", "search", "token", "refresh"}, want: []string{"--memory-search", "token refresh"}},
		{name: "legacy slash memory rag", args: []string{"memory/rag", "search", "token", "refresh"}, want: []string{"--memory-search", "token refresh"}},
		{name: "config alias", args: []string{"cfg", "validate"}, want: []string{"--validate-config"}},
		{name: "plugin domain alias", args: []string{"plugin", "describe", "runner"}, want: []string{"--describe-plugin", "runner"}},
		{name: "plugin mcp manifest command alias", args: []string{"plugins", "manifest", "mcp.yaml"}, want: []string{"--mcp-manifest", "mcp.yaml"}},
		{name: "plugin mcp tool command alias", args: []string{"plugins", "tool", "lookup"}, want: []string{"--mcp-tool", "lookup"}},
		{name: "plugin mcp method command alias", args: []string{"plugins", "method", "ping"}, want: []string{"--mcp-method", "ping"}},
		{name: "worktree alias", args: []string{"wt", "list"}, want: []string{"--list-worktrees"}},
		{name: "eval alias", args: []string{"evaluation", "replay-response", "fixture.json", "summarize"}, want: []string{"--replay-response", "fixture.json", "--once", "summarize"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := translateCLIArgs(tt.args)
			require.NoError(t, got.Err)
			require.False(t, got.Help)
			assert.Equal(t, tt.want, got.Args)
		})
	}
}

func TestTranslateCLIArgs_AllDocumentedCompatibilityCommandsRoute(t *testing.T) {
	t.Parallel()

	for _, domain := range cliHelpDomains {
		t.Run(domain.Name, func(t *testing.T) {
			t.Parallel()

			for _, command := range domain.Commands {
				if len(command.Legacy) == 0 {
					continue
				}

				t.Run(command.Name, func(t *testing.T) {
					t.Parallel()

					got := translateCLIArgs(documentedCommandArgsForTest(domain, command))
					require.NoError(t, got.Err)
					require.False(t, got.Help)
					assertCommandLegacyPrefix(t, command, got.Args)
				})
			}
		})
	}
}

func TestTranslateCLIArgs_AllDomainAliasesRouteDocumentedCommands(t *testing.T) {
	t.Parallel()

	for _, domain := range cliHelpDomains {
		domainTokens := append([]string{domain.Name}, domain.Aliases...)
		domainTokens = append(domainTokens, domain.HiddenAliases...)

		for _, domainToken := range domainTokens {
			t.Run(domain.Name+"/"+domainToken, func(t *testing.T) {
				t.Parallel()

				for _, command := range domain.Commands {
					t.Run(command.Name, func(t *testing.T) {
						t.Parallel()

						args := documentedCommandArgsForTokens(domainToken, command.Name, command)

						got := translateCLIArgs(args)
						require.NoError(t, got.Err)
						require.False(t, got.Help)
						assertCommandLegacyPrefix(t, command, got.Args)
					})
				}
			})
		}
	}
}

func TestTranslateCLIArgs_AllCommandAliasesRoute(t *testing.T) {
	t.Parallel()

	for _, domain := range cliHelpDomains {
		t.Run(domain.Name, func(t *testing.T) {
			t.Parallel()

			domainToken := domainTokenForTest(domain)
			for _, command := range domain.Commands {
				for _, alias := range command.Aliases {
					t.Run(command.Name+"/"+alias, func(t *testing.T) {
						t.Parallel()

						args := documentedCommandArgsForTokens(domainToken, alias, command)

						got := translateCLIArgs(args)
						require.NoError(t, got.Err)
						require.False(t, got.Help)
						assertCommandLegacyPrefix(t, command, got.Args)
					})
				}
			}
		})
	}
}

func TestTranslateCLIArgs_AllDocumentedCommandsParseWithRegisteredFlags(t *testing.T) {
	t.Parallel()

	for _, domain := range cliHelpDomains {
		t.Run(domain.Name, func(t *testing.T) {
			t.Parallel()

			for _, command := range domain.Commands {
				t.Run(command.Name, func(t *testing.T) {
					t.Parallel()

					fs := newRegisteredFlagSetForTest(t)
					args := documentedCommandArgsForTest(domain, command)

					got := translateCLIArgsWithFlagSet(args, fs)
					require.NoError(t, got.Err)
					require.False(t, got.Help)
					require.NoError(t, fs.Parse(got.Args), "translated args should parse: %#v", got.Args)
				})
			}
		})
	}
}

func TestTranslateCLIArgsWithFlagSet_PreservesParseableFlagOrder(t *testing.T) {
	t.Parallel()

	t.Run("string command value remains before follow-on flags", func(t *testing.T) {
		t.Parallel()

		fs := flag.NewFlagSet("test", flag.ContinueOnError)
		runPlugin := fs.String("run-plugin", "", "")
		pluginDryRun := fs.Bool("plugin-dry-run", false, "")

		got := translateCLIArgsWithFlagSet([]string{"plugins", "run", "reviewer/check", "--plugin-dry-run"}, fs)
		require.NoError(t, got.Err)
		require.False(t, got.Help)
		require.NoError(t, fs.Parse(got.Args))

		assert.Equal(t, "reviewer/check", *runPlugin)
		assert.True(t, *pluginDryRun)
	})

	t.Run("bool command keeps trailing flags parseable", func(t *testing.T) {
		t.Parallel()

		fs := flag.NewFlagSet("test", flag.ContinueOnError)
		model := fs.String("model", "", "")
		watchScan := fs.Bool("watch-scan", false, "")
		watchJSON := fs.Bool("watch-json", false, "")

		got := translateCLIArgsWithFlagSet([]string{"watch", "json", "--model", "test/model"}, fs)
		require.NoError(t, got.Err)
		require.False(t, got.Help)
		require.NoError(t, fs.Parse(got.Args))

		assert.True(t, *watchScan)
		assert.True(t, *watchJSON)
		assert.Equal(t, "test/model", *model)
	})

	t.Run("watch quality flags remain parseable after grouped command", func(t *testing.T) {
		t.Parallel()

		fs := flag.NewFlagSet("test", flag.ContinueOnError)
		watchScan := fs.Bool("watch-scan", false, "")
		watchBaseline := fs.String("watch-baseline", "", "")
		watchGate := fs.Bool("watch-gate", false, "")
		watchGateMinSeverity := fs.String("watch-gate-min-severity", "", "")
		watchUpsertIssues := fs.Bool("watch-upsert-issues", false, "")
		watchRepository := fs.String("watch-github-repository", "", "")

		got := translateCLIArgsWithFlagSet([]string{
			"watch", "scan",
			"--watch-baseline", ".atteler/watch-baseline.json",
			"--watch-gate",
			"--watch-gate-min-severity", "warning",
			"--watch-upsert-issues",
			"--watch-github-repository", "owner/repo",
		}, fs)
		require.NoError(t, got.Err)
		require.False(t, got.Help)
		require.NoError(t, fs.Parse(got.Args))

		assert.True(t, *watchScan)
		assert.Equal(t, ".atteler/watch-baseline.json", *watchBaseline)
		assert.True(t, *watchGate)
		assert.Equal(t, "warning", *watchGateMinSeverity)
		assert.True(t, *watchUpsertIssues)
		assert.Equal(t, "owner/repo", *watchRepository)
	})

	t.Run("joined prompt command keeps trailing flags parseable", func(t *testing.T) {
		t.Parallel()

		fs := flag.NewFlagSet("test", flag.ContinueOnError)
		oncePrompt := fs.String("once", "", "")
		model := fs.String("model", "", "")
		output := fs.String("output", "", "")

		var autonomy autonomyFlag

		fs.Var(&autonomy, "autonomy", "")

		got := translateCLIArgsWithFlagSet([]string{"chat", "once", "explain", "this", "repo", "--autonomy", "low", "--model", "test/model", "--output", "json"}, fs)
		require.NoError(t, got.Err)
		require.False(t, got.Help)
		require.NoError(t, fs.Parse(got.Args))

		assert.Equal(t, "explain this repo", *oncePrompt)
		assert.Equal(t, "low", autonomy.String())
		assert.Equal(t, "test/model", *model)
		assert.Equal(t, "json", *output)
	})

	t.Run("leading bool flag still allows domain detection", func(t *testing.T) {
		t.Parallel()

		fs := flag.NewFlagSet("test", flag.ContinueOnError)
		headless := fs.Bool("headless", false, "")
		model := fs.String("model", "", "")
		once := fs.String("once", "", "")

		got := translateCLIArgsWithFlagSet([]string{"--headless", "chat", "once", "explain", "--model", "test/model"}, fs)
		require.NoError(t, got.Err)
		require.False(t, got.Help)
		require.NoError(t, fs.Parse(got.Args))

		assert.True(t, *headless)
		assert.Equal(t, "explain", *once)
		assert.Equal(t, "test/model", *model)
	})

	t.Run("headless list recover and cleanup commands map to compatibility flags", func(t *testing.T) {
		t.Parallel()

		opts, fs := newCLIOptionsAndFlagSetForTest(t)
		got := translateCLIArgsWithFlagSet([]string{"session", "headless", "--headless-status", "failed", "--headless-max-age", "24h"}, fs)
		require.NoError(t, got.Err)
		require.False(t, got.Help)
		require.NoError(t, fs.Parse(got.Args))

		assert.True(t, opts.listHeadless)
		assert.Equal(t, "failed", opts.headlessStatusFilter)
		assert.Equal(t, "24h", opts.headlessMaxAge)

		opts, fs = newCLIOptionsAndFlagSetForTest(t)
		got = translateCLIArgsWithFlagSet([]string{"session", "recover-headless"}, fs)
		require.NoError(t, got.Err)
		require.False(t, got.Help)
		require.NoError(t, fs.Parse(got.Args))

		assert.True(t, opts.recoverHeadless)

		opts, fs = newCLIOptionsAndFlagSetForTest(t)
		got = translateCLIArgsWithFlagSet([]string{"session", "cleanup-headless", "--headless-max-age", "168h"}, fs)
		require.NoError(t, got.Err)
		require.False(t, got.Help)
		require.NoError(t, fs.Parse(got.Args))

		assert.True(t, opts.cleanupHeadless)
		assert.Equal(t, "168h", opts.headlessMaxAge)
	})

	t.Run("headless ID commands map IDs to compatibility flags", func(t *testing.T) {
		t.Parallel()

		opts, fs := newCLIOptionsAndFlagSetForTest(t)
		got := translateCLIArgsWithFlagSet([]string{"session", "status-headless", "run-123"}, fs)
		require.NoError(t, got.Err)
		require.False(t, got.Help)
		require.NoError(t, fs.Parse(got.Args))
		assert.Equal(t, "run-123", opts.statusHeadlessID)

		opts, fs = newCLIOptionsAndFlagSetForTest(t)
		got = translateCLIArgsWithFlagSet([]string{"session", "cancel-headless", "run-456"}, fs)
		require.NoError(t, got.Err)
		require.False(t, got.Help)
		require.NoError(t, fs.Parse(got.Args))
		assert.Equal(t, "run-456", opts.cancelHeadlessID)

		opts, fs = newCLIOptionsAndFlagSetForTest(t)
		got = translateCLIArgsWithFlagSet([]string{"session", "retry-headless", "run-654", "--retry-headless-id", "run-654-retry"}, fs)
		require.NoError(t, got.Err)
		require.False(t, got.Help)
		require.NoError(t, fs.Parse(got.Args))
		assert.Equal(t, "run-654", opts.retryHeadlessID)
		assert.Equal(t, "run-654-retry", opts.retryHeadlessNewID)

		opts, fs = newCLIOptionsAndFlagSetForTest(t)
		got = translateCLIArgsWithFlagSet([]string{"session", "stream-headless", "run-789"}, fs)
		require.NoError(t, got.Err)
		require.False(t, got.Help)
		require.NoError(t, fs.Parse(got.Args))
		assert.Equal(t, "run-789", opts.streamHeadlessID)
	})

	t.Run("headless ID commands reject missing IDs", func(t *testing.T) {
		t.Parallel()

		tests := []struct {
			name string
			args []string
		}{
			{name: "status", args: []string{"session", "status-headless"}},
			{name: "cancel", args: []string{"session", "cancel-headless"}},
			{name: "retry", args: []string{"session", "retry-headless"}},
			{name: "stream", args: []string{"session", "stream-headless"}},
		}

		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				t.Parallel()

				_, fs := newCLIOptionsAndFlagSetForTest(t)
				got := translateCLIArgsWithFlagSet(tt.args, fs)

				require.Error(t, got.Err)
				assert.Contains(t, got.Err.Error(), tt.args[1]+" requires <id>")
				assert.Contains(t, got.Err.Error(), "atteler help session")
			})
		}
	})

	t.Run("headless lifecycle flags parse with one-shot command", func(t *testing.T) {
		t.Parallel()

		opts, fs := newCLIOptionsAndFlagSetForTest(t)
		got := translateCLIArgsWithFlagSet([]string{"chat", "once", "explain", "--headless", "--headless-id", "run-known", "--headless-private-log"}, fs)
		require.NoError(t, got.Err)
		require.False(t, got.Help)
		require.NoError(t, fs.Parse(got.Args))

		assert.True(t, opts.headless)
		assert.Equal(t, "run-known", opts.headlessID)
		assert.True(t, opts.headlessPrivateLog)
		assert.Equal(t, "explain", opts.oncePrompt)
	})

	t.Run("opaque shell command keeps dash-prefixed command args", func(t *testing.T) {
		t.Parallel()

		fs := flag.NewFlagSet("test", flag.ContinueOnError)
		bashCommand := fs.String("bash", "", "")

		got := translateCLIArgsWithFlagSet([]string{"agents", "bash", "go", "test", "./cmd/atteler", "-run", "TestCLI"}, fs)
		require.NoError(t, got.Err)
		require.False(t, got.Help)
		require.NoError(t, fs.Parse(got.Args))

		assert.Equal(t, "go test ./cmd/atteler -run TestCLI", *bashCommand)
	})

	t.Run("domain run delimiter keeps dash prompt parseable", func(t *testing.T) {
		t.Parallel()

		fs := flag.NewFlagSet("test", flag.ContinueOnError)
		worktree := fs.Bool("worktree", false, "")

		got := translateCLIArgsWithFlagSet([]string{"worktrees", "run", "--", "--keep", "alive"}, fs)
		require.NoError(t, got.Err)
		require.False(t, got.Help)
		require.NoError(t, fs.Parse(got.Args))

		assert.True(t, *worktree)
		assert.Equal(t, []string{"--keep alive"}, fs.Args())
	})
}

func TestTranslateCLIArgs_DomainHelpAndUnknownCommand(t *testing.T) {
	t.Parallel()

	help := translateCLIArgs([]string{"help", "code-intel"})
	require.NoError(t, help.Err)
	assert.True(t, help.Help)
	assert.Equal(t, "code-intel", help.HelpDomain)

	domainHelp := translateCLIArgs([]string{"memory", "--help"})
	require.NoError(t, domainHelp.Err)
	assert.True(t, domainHelp.Help)
	assert.Equal(t, "memory", domainHelp.HelpDomain)

	aliasHelp := translateCLIArgs([]string{"help", "session"})
	require.NoError(t, aliasHelp.Err)
	assert.True(t, aliasHelp.Help)
	assert.Equal(t, "session", aliasHelp.HelpDomain)

	aliasCommandHelp := translateCLIArgs([]string{"session", "list", "--help"})
	require.NoError(t, aliasCommandHelp.Err)
	assert.True(t, aliasCommandHelp.Help)
	assert.Equal(t, "session", aliasCommandHelp.HelpDomain)

	commandHelp := translateCLIArgs([]string{"code-intel", "summary", "--help"})
	require.NoError(t, commandHelp.Err)
	assert.True(t, commandHelp.Help)
	assert.Equal(t, "code-intel", commandHelp.HelpDomain)

	commandHelpAfterFlags := translateCLIArgs([]string{"code-intel", "summary", "--model", "test/model", "--help"})
	require.NoError(t, commandHelpAfterFlags.Err)
	assert.True(t, commandHelpAfterFlags.Help)
	assert.Equal(t, "code-intel", commandHelpAfterFlags.HelpDomain)

	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	fs.Bool("code-summary", false, "")

	legacyFlagHelp := translateCLIArgsWithFlagSet([]string{"help", "--code-summary"}, fs)
	require.NoError(t, legacyFlagHelp.Err)
	assert.True(t, legacyFlagHelp.Help)
	assert.Equal(t, "code-intel", legacyFlagHelp.HelpDomain)

	scopedLegacyFlagHelp := translateCLIArgs([]string{"code-intel", "--code-summary", "--help"})
	require.NoError(t, scopedLegacyFlagHelp.Err)
	assert.True(t, scopedLegacyFlagHelp.Help)
	assert.Equal(t, "code-intel", scopedLegacyFlagHelp.HelpDomain)

	scopedLegacyFlagHelpWithAlias := translateCLIArgs([]string{"session", "--session", "--help"})
	require.NoError(t, scopedLegacyFlagHelpWithAlias.Err)
	assert.True(t, scopedLegacyFlagHelpWithAlias.Help)
	assert.Equal(t, "session", scopedLegacyFlagHelpWithAlias.HelpDomain)

	topLevelHelp := translateCLIArgs([]string{"help", "--help"})
	require.NoError(t, topLevelHelp.Err)
	assert.True(t, topLevelHelp.Help)
	assert.Empty(t, topLevelHelp.HelpDomain)

	helpHelp := translateCLIArgs([]string{"help", "help"})
	require.NoError(t, helpHelp.Err)
	assert.True(t, helpHelp.Help)
	assert.Empty(t, helpHelp.HelpDomain)

	legacyHelp := translateCLIArgs([]string{"help", "legacy"})
	require.NoError(t, legacyHelp.Err)
	assert.True(t, legacyHelp.Help)
	assert.Equal(t, "legacy", legacyHelp.HelpDomain)

	overviewHelp := translateCLIArgs([]string{"help", "overview"})
	require.NoError(t, overviewHelp.Err)
	assert.True(t, overviewHelp.Help)
	assert.Equal(t, "overview", overviewHelp.HelpDomain)

	unknownHelp := translateCLIArgs([]string{"help", "wat"})
	require.NoError(t, unknownHelp.Err)
	assert.True(t, unknownHelp.Help)
	assert.Equal(t, "wat", unknownHelp.HelpDomain)

	flagHelp := translateCLIArgs([]string{"--help"})
	require.NoError(t, flagHelp.Err)
	assert.True(t, flagHelp.Help)
	assert.Empty(t, flagHelp.HelpDomain)

	goFlagHelp := translateCLIArgs([]string{"-help"})
	require.NoError(t, goFlagHelp.Err)
	assert.True(t, goFlagHelp.Help)
	assert.Empty(t, goFlagHelp.HelpDomain)

	trailingFlagHelp := translateCLIArgs([]string{"--model", "test/model", "--help"})
	require.NoError(t, trailingFlagHelp.Err)
	assert.True(t, trailingFlagHelp.Help)
	assert.Empty(t, trailingFlagHelp.HelpDomain)

	for _, test := range []struct {
		name string
		want string
	}{
		{name: "code-intel", want: "unknown code-intel command \"wat\"; run `atteler help code-intel`"},
		{name: "codeintel", want: "unknown codeintel command \"wat\"; run `atteler help codeintel`"},
		{name: "code-intelligence", want: "unknown code-intelligence command \"wat\"; run `atteler help code-intelligence`"},
	} {
		t.Run("unknown code-intel command/"+test.name, func(t *testing.T) {
			t.Parallel()

			unknownCodeIntel := translateCLIArgs([]string{test.name, "wat"})
			require.EqualError(t, unknownCodeIntel.Err, test.want)
			assert.False(t, unknownCodeIntel.Help)
			assert.Empty(t, unknownCodeIntel.Args)
		})
	}

	unknownCodePrompt := translateCLIArgs([]string{"code", "wat"})
	require.NoError(t, unknownCodePrompt.Err)
	assert.False(t, unknownCodePrompt.Help)
	assert.Equal(t, []string{"code", "wat"}, unknownCodePrompt.Args)

	registeredFlags := newRegisteredFlagSetForTest(t)

	unknownCodeIntelWithFlag := translateCLIArgsWithFlagSet([]string{"code-intel", "--json", "wat"}, registeredFlags)
	require.EqualError(t, unknownCodeIntelWithFlag.Err, "unknown code-intel command \"wat\"; run `atteler help code-intel`")
	assert.False(t, unknownCodeIntelWithFlag.Help)
	assert.Empty(t, unknownCodeIntelWithFlag.Args)

	unknownCodePromptWithFlag := translateCLIArgsWithFlagSet([]string{"code", "--json", "wat"}, registeredFlags)
	require.NoError(t, unknownCodePromptWithFlag.Err)
	assert.False(t, unknownCodePromptWithFlag.Help)
	assert.Equal(t, []string{"code", "--json", "wat"}, unknownCodePromptWithFlag.Args)

	unknownCodeIntelFlag := translateCLIArgsWithFlagSet([]string{"code-intel", "--wat", "summary"}, registeredFlags)
	require.EqualError(t, unknownCodeIntelFlag.Err, "unknown code-intel flag \"--wat\"; run `atteler help code-intel`")
	assert.False(t, unknownCodeIntelFlag.Help)
	assert.Empty(t, unknownCodeIntelFlag.Args)

	unknownCodeIntelFlagAfterOutputFlag := translateCLIArgsWithFlagSet([]string{"code-intel", "--json", "--wat", "summary"}, registeredFlags)
	require.EqualError(t, unknownCodeIntelFlagAfterOutputFlag.Err, "unknown code-intel flag \"--wat\"; run `atteler help code-intel`")
	assert.False(t, unknownCodeIntelFlagAfterOutputFlag.Help)
	assert.Empty(t, unknownCodeIntelFlagAfterOutputFlag.Args)

	unknownCodeIntelCommandTailFlag := translateCLIArgsWithFlagSet([]string{"code-intel", "summary", "--wat"}, registeredFlags)
	require.EqualError(t, unknownCodeIntelCommandTailFlag.Err, "unknown code-intel flag \"--wat\"; run `atteler help code-intel`")
	assert.False(t, unknownCodeIntelCommandTailFlag.Help)
	assert.Empty(t, unknownCodeIntelCommandTailFlag.Args)

	unknownCodeIntelCommandTailFlagBeforeRequiredArg := translateCLIArgsWithFlagSet([]string{"code-intel", "symbol", "--wat"}, registeredFlags)
	require.EqualError(t, unknownCodeIntelCommandTailFlagBeforeRequiredArg.Err, "unknown code-intel flag \"--wat\"; run `atteler help code-intel`")
	assert.False(t, unknownCodeIntelCommandTailFlagBeforeRequiredArg.Help)
	assert.Empty(t, unknownCodeIntelCommandTailFlagBeforeRequiredArg.Args)

	unknownCodeIntelCommandTailFlagAfterRequiredArg := translateCLIArgsWithFlagSet([]string{"code-intel", "symbol", "Run", "--wat"}, registeredFlags)
	require.EqualError(t, unknownCodeIntelCommandTailFlagAfterRequiredArg.Err, "unknown code-intel flag \"--wat\"; run `atteler help code-intel`")
	assert.False(t, unknownCodeIntelCommandTailFlagAfterRequiredArg.Help)
	assert.Empty(t, unknownCodeIntelCommandTailFlagAfterRequiredArg.Args)

	unknownCodeIntelCommandTailFlagAfterScopedPrefix := translateCLIArgsWithFlagSet([]string{"code-intel", "--json", "summary", "--wat"}, registeredFlags)
	require.EqualError(t, unknownCodeIntelCommandTailFlagAfterScopedPrefix.Err, "unknown code-intel flag \"--wat\"; run `atteler help code-intel`")
	assert.False(t, unknownCodeIntelCommandTailFlagAfterScopedPrefix.Help)
	assert.Empty(t, unknownCodeIntelCommandTailFlagAfterScopedPrefix.Args)

	delimitedCodeIntelDashValue := translateCLIArgsWithFlagSet([]string{"code-intel", "symbol", "--", "--generated-name"}, registeredFlags)
	require.NoError(t, delimitedCodeIntelDashValue.Err)
	assert.False(t, delimitedCodeIntelDashValue.Help)
	assert.Equal(t, []string{"--code-symbol", "--generated-name"}, delimitedCodeIntelDashValue.Args)

	unknownCodePromptFlag := translateCLIArgsWithFlagSet([]string{"code", "--wat", "summary"}, registeredFlags)
	require.NoError(t, unknownCodePromptFlag.Err)
	assert.False(t, unknownCodePromptFlag.Help)
	assert.Equal(t, []string{"code", "--wat", "summary"}, unknownCodePromptFlag.Args)

	missingCodeIntelCommandWithFlag := translateCLIArgsWithFlagSet([]string{"code-intel", "--json"}, registeredFlags)
	require.EqualError(t, missingCodeIntelCommandWithFlag.Err, "code-intel requires a command; run `atteler help code-intel`")
	assert.False(t, missingCodeIntelCommandWithFlag.Help)
	assert.Empty(t, missingCodeIntelCommandWithFlag.Args)

	missingCodeIntelCommandWithOutputFlag := translateCLIArgsWithFlagSet([]string{"code-intel", "--output", "json"}, registeredFlags)
	require.EqualError(t, missingCodeIntelCommandWithOutputFlag.Err, "code-intel requires a command; run `atteler help code-intel`")
	assert.False(t, missingCodeIntelCommandWithOutputFlag.Help)
	assert.Empty(t, missingCodeIntelCommandWithOutputFlag.Args)

	missingCodeIntelCommandWithDisabledSelector := translateCLIArgsWithFlagSet([]string{"code-intel", "--code-summary=false"}, registeredFlags)
	require.EqualError(t, missingCodeIntelCommandWithDisabledSelector.Err, "code-intel requires a command; run `atteler help code-intel`")
	assert.False(t, missingCodeIntelCommandWithDisabledSelector.Help)
	assert.Empty(t, missingCodeIntelCommandWithDisabledSelector.Args)

	missingCodeIntelCommandWithEmptySelector := translateCLIArgsWithFlagSet([]string{"code-intel", "--code-symbol="}, registeredFlags)
	require.EqualError(t, missingCodeIntelCommandWithEmptySelector.Err, "code-intel requires a command; run `atteler help code-intel`")
	assert.False(t, missingCodeIntelCommandWithEmptySelector.Help)
	assert.Empty(t, missingCodeIntelCommandWithEmptySelector.Args)

	missingCodeIntelCommandWithEmptySeparateSelector := translateCLIArgsWithFlagSet([]string{"code-intel", "--code-symbol", ""}, registeredFlags)
	require.EqualError(t, missingCodeIntelCommandWithEmptySeparateSelector.Err, "code-intel requires a command; run `atteler help code-intel`")
	assert.False(t, missingCodeIntelCommandWithEmptySeparateSelector.Help)
	assert.Empty(t, missingCodeIntelCommandWithEmptySeparateSelector.Args)

	missingCodeIntelCommandWithEmptyLSPSelector := translateCLIArgsWithFlagSet([]string{"code-intel", "--lsp-workspace-symbols", ""}, registeredFlags)
	require.EqualError(t, missingCodeIntelCommandWithEmptyLSPSelector.Err, "code-intel requires a command; run `atteler help code-intel`")
	assert.False(t, missingCodeIntelCommandWithEmptyLSPSelector.Help)
	assert.Empty(t, missingCodeIntelCommandWithEmptyLSPSelector.Args)

	scopedCodeIntelLegacyFlag := translateCLIArgsWithFlagSet([]string{"code-intel", "--json", "--code-summary"}, registeredFlags)
	require.NoError(t, scopedCodeIntelLegacyFlag.Err)
	assert.False(t, scopedCodeIntelLegacyFlag.Help)
	assert.Equal(t, []string{"--json", "--code-summary"}, scopedCodeIntelLegacyFlag.Args)

	scopedCodeIntelLegacyOutputFlag := translateCLIArgsWithFlagSet([]string{"code-intel", "--output", "json", "--code-summary"}, registeredFlags)
	require.NoError(t, scopedCodeIntelLegacyOutputFlag.Err)
	assert.False(t, scopedCodeIntelLegacyOutputFlag.Help)
	assert.Equal(t, []string{"--output", "json", "--code-summary"}, scopedCodeIntelLegacyOutputFlag.Args)

	scopedCodeIntelLSPLegacyFlag := translateCLIArgsWithFlagSet([]string{"code-intel", "--json", "--lsp-symbols", "--lsp-file", "main.go"}, registeredFlags)
	require.NoError(t, scopedCodeIntelLSPLegacyFlag.Err)
	assert.False(t, scopedCodeIntelLSPLegacyFlag.Help)
	assert.Equal(t, []string{"--json", "--lsp-symbols", "--lsp-file", "main.go"}, scopedCodeIntelLSPLegacyFlag.Args)

	scopedCodeIntelLSPWorkspaceLegacyFlag := translateCLIArgsWithFlagSet([]string{"code-intel", "--output", "json", "--lsp-workspace-symbols", "Handler"}, registeredFlags)
	require.NoError(t, scopedCodeIntelLSPWorkspaceLegacyFlag.Err)
	assert.False(t, scopedCodeIntelLSPWorkspaceLegacyFlag.Help)
	assert.Equal(t, []string{"--output", "json", "--lsp-workspace-symbols", "Handler"}, scopedCodeIntelLSPWorkspaceLegacyFlag.Args)

	unknownAlias := translateCLIArgs([]string{"session", "wat"})
	require.NoError(t, unknownAlias.Err)
	assert.False(t, unknownAlias.Help)
	assert.Equal(t, []string{"session", "wat"}, unknownAlias.Args)
}

func TestTranslateCLIArgs_HelpPreservesAllDomainAliases(t *testing.T) {
	t.Parallel()

	for _, domain := range cliHelpDomains {
		tokens := append([]string{domain.Name}, domain.Aliases...)
		tokens = append(tokens, domain.HiddenAliases...)

		for _, token := range tokens {
			t.Run(domain.Name+"/"+token, func(t *testing.T) {
				t.Parallel()

				helpCommand := translateCLIArgs([]string{"help", token})
				require.NoError(t, helpCommand.Err)
				require.True(t, helpCommand.Help)
				assert.Equal(t, token, helpCommand.HelpDomain)

				domainHelp := translateCLIArgs([]string{token, "--help"})
				require.NoError(t, domainHelp.Err)
				require.True(t, domainHelp.Help)
				assert.Equal(t, token, domainHelp.HelpDomain)

				domainHelpCommand := translateCLIArgs([]string{token, "help"})
				require.NoError(t, domainHelpCommand.Err)
				require.True(t, domainHelpCommand.Help)
				assert.Equal(t, token, domainHelpCommand.HelpDomain)

				legacy := firstLegacyAlias(domain)
				require.NotEmpty(t, legacy, "domain %s should document at least one legacy alias", domain.Name)

				scopedLegacyHelp := translateCLIArgs([]string{token, legacy, "--help"})
				require.NoError(t, scopedLegacyHelp.Err)
				require.True(t, scopedLegacyHelp.Help)
				assert.Equal(t, token, scopedLegacyHelp.HelpDomain)
			})
		}
	}
}

func TestTranslateCLIArgs_HelpPrefixWithUnknownSelectorStaysPrompt(t *testing.T) {
	t.Parallel()

	got := translateCLIArgs([]string{"--model", "test/model", "help", "me", "write", "tests"})
	require.NoError(t, got.Err)
	assert.False(t, got.Help)
	assert.Equal(t, []string{"--model", "test/model", "help", "me", "write", "tests"}, got.Args)
}

func TestTranslateCLIArgs_OpaqueCommandHelpIsCommandData(t *testing.T) {
	t.Parallel()

	got := translateCLIArgs([]string{"agents", "bash", "go", "test", "./cmd/atteler", "-run", "TestCLI", "--help"})
	require.NoError(t, got.Err)
	assert.False(t, got.Help)
	assert.Equal(t, []string{"--bash", "go test ./cmd/atteler -run TestCLI --help"}, got.Args)
}

func TestTranslateCLIArgs_DelimitedHelpIsCommandData(t *testing.T) {
	t.Parallel()

	got := translateCLIArgs([]string{"chat", "run", "--", "--help", "me"})
	require.NoError(t, got.Err)
	assert.False(t, got.Help)
	assert.Equal(t, []string{"--", "--help me"}, got.Args)
}

func TestTranslateCLIArgs_BareHelpAfterCommandIsCommandData(t *testing.T) {
	t.Parallel()

	got := translateCLIArgs([]string{"memory", "search", "help"})
	require.NoError(t, got.Err)
	assert.False(t, got.Help)
	assert.Equal(t, []string{"--memory-search", "help"}, got.Args)
}

func TestTranslateCLIArgs_RequiredCommandArgsDoNotConsumeTrailingFlags(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		want string
		args []string
	}{
		{
			name: "string alias command with only flags",
			args: []string{"plugins", "run", "--plugin-dry-run"},
			want: "plugins run requires <plugin[/entrypoint]>",
		},
		{
			name: "chat once with disabled stdin and no prompt",
			args: []string{"chat", "once", "--stdin=false"},
			want: "chat once requires <prompt|--stdin>",
		},
		{
			name: "session command with only flags",
			args: []string{"session", "export", "--export-format", "json"},
			want: "session export requires <id-or-path>",
		},
		{
			name: "code-intel command with only output flag",
			args: []string{"code-intel", "symbol", "--json"},
			want: "code-intel symbol requires <name>",
		},
		{
			name: "issue implement with only flags",
			args: []string{"issue", "implement", "--open-pr"},
			want: "issue implement requires <issue-ref>",
		},
		{
			name: "opaque shell command with no command",
			args: []string{"agents", "bash"},
			want: "agents bash requires <command>",
		},
		{
			name: "delimiter without positional value",
			args: []string{"eval", "output", "--"},
			want: "eval output requires <path>",
		},
		{
			name: "prompt-after-value command without prompt",
			args: []string{"eval", "replay-response", "fixture.json", "--output", "json"},
			want: "eval replay-response requires <path> <prompt|--stdin>",
		},
		{
			name: "prompt-after-value command without path",
			args: []string{"eval", "replay-response", "--stdin"},
			want: "eval replay-response requires <path> <prompt|--stdin>",
		},
		{
			name: "prompt-after-value command with disabled stdin",
			args: []string{"eval", "replay-response", "fixture.json", "--stdin=false"},
			want: "eval replay-response requires <path> <prompt|--stdin>",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			fs := flag.NewFlagSet("test", flag.ContinueOnError)
			fs.Bool("plugin-dry-run", false, "")
			fs.String("run-plugin", "", "")
			fs.String("export-session", "", "")
			fs.String("export-format", "markdown", "")
			fs.String("code-symbol", "", "")
			fs.Bool("json", false, "")
			fs.String("issue-implement", "", "")
			fs.Bool("open-pr", false, "")
			fs.String("bash", "", "")
			fs.String("eval-output", "", "")

			got := translateCLIArgsWithFlagSet(tt.args, fs)
			require.Error(t, got.Err)
			assert.Contains(t, got.Err.Error(), tt.want)
			assert.False(t, got.Help)
			assert.Empty(t, got.Args)
		})
	}
}
