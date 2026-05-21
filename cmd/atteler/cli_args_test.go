package main

import (
	"flag"
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
		{name: "config", args: []string{"config", "validate"}, want: []string{"--validate-config"}},
		{name: "providers", args: []string{"providers", "list"}, want: []string{"--list-providers"}},
		{name: "agents", args: []string{"agents", "plan", "review", "auth"}, want: []string{"--plan-agents", "review auth"}},
		{name: "agents performance", args: []string{"agents", "performance"}, want: []string{"--agent-performance-summary"}},
		{name: "agents feedback apply", args: []string{"agents", "feedback-apply", "agents.yaml"}, want: []string{"--feedback-apply-config", "agents.yaml"}},
		{name: "agents bash", args: []string{"agents", "bash", "go", "test", "./cmd/atteler"}, want: []string{"--bash", "go test ./cmd/atteler"}},
		{name: "memory rag", args: []string{"memory", "search", "OAuth", "retry"}, want: []string{"--memory-search", "OAuth retry"}},
		{name: "code intel", args: []string{"code-intel", "summary"}, want: []string{"--code-summary"}},
		{name: "review", args: []string{"review", "scan"}, want: []string{"--review-scan"}},
		{name: "watch", args: []string{"watch", "json"}, want: []string{"--watch-scan", "--watch-json"}},
		{name: "plugins", args: []string{"plugins", "describe", "reviewer"}, want: []string{"--describe-plugin", "reviewer"}},
		{name: "worktrees", args: []string{"worktrees", "merge", "session-123"}, want: []string{"--merge-worktree", "session-123"}},
		{name: "worktrees merge base mismatch override", args: []string{"worktrees", "merge", "session-123", "--merge-worktree-allow-base-mismatch"}, want: []string{"--merge-worktree", "session-123", "--merge-worktree-allow-base-mismatch"}},
		{name: "worktrees run prompt", args: []string{"worktrees", "run", "add", "tests"}, want: []string{"--worktree", "add tests"}},
		{name: "eval", args: []string{"eval", "output", "actual.txt"}, want: []string{"--eval-output", "actual.txt"}},
		{name: "eval list", args: []string{"eval", "list"}, want: []string{"--list-evaluations"}},
		{name: "eval record response with prompt", args: []string{"eval", "record-response", "fixture.json", "summarize", "readme"}, want: []string{"--record-response", "fixture.json", "--once", "summarize readme"}},
		{name: "eval replay response with prompt and flags", args: []string{"eval", "replay-response", "fixture.json", "summarize", "readme", "--output", "json"}, want: []string{"--replay-response", "fixture.json", "--once", "summarize readme", "--output", "json"}},
		{name: "eval replay response with stdin", args: []string{"eval", "replay-response", "fixture.json", "--stdin"}, want: []string{"--replay-response", "fixture.json", "--stdin"}},
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

func TestTranslateCLIArgs_CanonicalDomainsAndAliasesRoute(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		args []string
		want []string
	}{
		{name: "canonical slash chat session", args: []string{"chat/session", "once", "hello"}, want: []string{"--once", "hello"}},
		{name: "canonical slash memory rag", args: []string{"memory/rag", "search", "token", "refresh"}, want: []string{"--memory-search", "token refresh"}},
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

	t.Run("joined prompt command keeps trailing flags parseable", func(t *testing.T) {
		t.Parallel()

		fs := flag.NewFlagSet("test", flag.ContinueOnError)
		oncePrompt := fs.String("once", "", "")
		model := fs.String("model", "", "")
		output := fs.String("output", "", "")

		got := translateCLIArgsWithFlagSet([]string{"chat", "once", "explain", "this", "repo", "--model", "test/model", "--output", "json"}, fs)
		require.NoError(t, got.Err)
		require.False(t, got.Help)
		require.NoError(t, fs.Parse(got.Args))

		assert.Equal(t, "explain this repo", *oncePrompt)
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

	unknown := translateCLIArgs([]string{"code-intel", "wat"})
	require.NoError(t, unknown.Err)
	assert.False(t, unknown.Help)
	assert.Equal(t, []string{"code-intel", "wat"}, unknown.Args)

	unknownAlias := translateCLIArgs([]string{"session", "wat"})
	require.NoError(t, unknownAlias.Err)
	assert.False(t, unknownAlias.Help)
	assert.Equal(t, []string{"session", "wat"}, unknownAlias.Args)
}

func TestTranslateCLIArgs_HelpPreservesAllDomainAliases(t *testing.T) {
	t.Parallel()

	for _, domain := range cliHelpDomains {
		for _, token := range append([]string{domain.Name}, domain.Aliases...) {
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
