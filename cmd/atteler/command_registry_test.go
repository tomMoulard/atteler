package main

import (
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	testDomainChatSession    = "chat/session"
	testDomainConfig         = "config"
	testDomainProviders      = "providers"
	testDomainPlugins        = "plugins"
	testDomainWorktrees      = "worktrees"
	testDomainEval           = "eval"
	testCommandList          = "list"
	testCommandDoctorOffline = "doctor-offline"
	testCommandRun           = "run"
	testCommandTemplate      = "template"
	testCommandVersion       = "version"
)

func TestCommandRegistry_ModularGroupsAreWellFormed(t *testing.T) {
	t.Parallel()

	registry := buildCommandRegistry()
	require.NotEmpty(t, registry)

	seen := make(map[string]bool, len(registry))
	seenTiers := make(map[commandTier]bool)

	for _, command := range registry {
		require.NotEmpty(t, command.name)
		assert.False(t, seen[command.name], "duplicate command registry name %q", command.name)
		seen[command.name] = true
		seenTiers[command.tier] = true

		assert.NotNil(t, command.match, "command %q should declare a matcher", command.name)

		switch command.tier {
		case tierProviderless:
			assert.NotNil(t, command.runProviderless, "providerless command %q should declare a providerless runner", command.name)
		case tierProviderlessConfig:
			assert.NotNil(t, command.runProviderlessConfig, "providerless-config command %q should declare a providerless-config runner", command.name)
		case tierStateful:
			assert.NotNil(t, command.runStateful, "stateful command %q should declare a stateful runner", command.name)
		default:
			require.Failf(t, "unexpected tier", "command %q has unexpected tier %d", command.name, command.tier)
		}
	}

	assert.True(t, seenTiers[tierProviderless], "registry should keep providerless commands modularized")
	assert.True(t, seenTiers[tierProviderlessConfig], "registry should keep providerless-config commands modularized")
	assert.True(t, seenTiers[tierStateful], "registry should keep stateful commands modularized")
}

func TestCommandRegistry_ModularGroupsPreserveImportantDispatchOrder(t *testing.T) {
	t.Parallel()

	registry := buildCommandRegistry()

	assertCommandBefore(t, registry, "mcp-invoke", "mcp-manifest")
	assertCommandBefore(t, registry, "speculate-plan", "speculate-run")
	assertCommandBefore(t, registry, "review-plan", "review-run")
	assertCommandBefore(t, registry, "list-agents", "code-intel")
	assertCommandBefore(t, registry, "watch-scan-providerless", "review-scan-providerless")
	assertCommandBefore(t, registry, "session-write", "async-run")
	assertCommandBefore(t, registry, "agent-memory", "list-models")
}

func TestCommandRegistry_TopLevelRegistryStaysSmall(t *testing.T) {
	t.Parallel()

	registry := buildCommandRegistry()
	assert.LessOrEqual(t, len(registry), 50, "top-level command registry should stay grouped by domain instead of one entry per flag")
}

func TestCommandRegistry_GroupedCommandsReachExpectedHandlers(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		wantName string
		args     []string
		wantTier commandTier
	}{
		{
			name:     "session list routes providerless",
			args:     []string{"session", testCommandList},
			wantName: "list-sessions",
			wantTier: tierProviderless,
		},
		{
			name:     "agents list routes providerless config",
			args:     []string{"agents", testCommandList},
			wantName: "list-agents",
			wantTier: tierProviderlessConfig,
		},
		{
			name:     "config doctor routes stateful diagnostics",
			args:     []string{"config", "doctor"},
			wantName: "doctor",
			wantTier: tierStateful,
		},
		{
			name:     "providers models routes stateful provider inventory",
			args:     []string{"providers", "models"},
			wantName: "list-models",
			wantTier: tierStateful,
		},
		{
			name:     "memory search routes providerless",
			args:     []string{"memory", "search", "hello", "auth"},
			wantName: "memory-command",
			wantTier: tierProviderless,
		},
		{
			name:     "code-intel summary routes providerless config",
			args:     []string{"code-intel", "summary"},
			wantName: "code-intel",
			wantTier: tierProviderlessConfig,
		},
		{
			name:     "review scan routes local analysis",
			args:     []string{"review", "scan"},
			wantName: "review-scan-providerless",
			wantTier: tierProviderlessConfig,
		},
		{
			name:     "watch json routes watch scan",
			args:     []string{"watch", "json"},
			wantName: "watch-scan-providerless",
			wantTier: tierProviderlessConfig,
		},
		{
			name:     "plugins manifest routes providerless MCP manifest",
			args:     []string{"plugins", "manifest", "mcp.yaml"},
			wantName: "mcp-manifest",
			wantTier: tierProviderless,
		},
		{
			name:     "plugins tool with manifest routes MCP invoke first",
			args:     []string{"plugins", "tool", "lookup", "--mcp-manifest", "mcp.yaml"},
			wantName: "mcp-invoke",
			wantTier: tierProviderless,
		},
		{
			name:     "plugins tool without manifest still routes MCP invoke",
			args:     []string{"plugins", "tool", "lookup"},
			wantName: "mcp-invoke",
			wantTier: tierProviderless,
		},
		{
			name:     "plugins method without manifest still routes MCP invoke",
			args:     []string{"plugins", "method", "ping"},
			wantName: "mcp-invoke",
			wantTier: tierProviderless,
		},
		{
			name:     "providers route-interactive routes model ranking",
			args:     []string{"providers", "route-interactive", "--route-candidate", "openai/gpt-fast,input=0.001,output=0.002,max=1000"},
			wantName: "route-models-providerless",
			wantTier: tierProviderless,
		},
		{
			name:     "providers route-interactive without candidates stays providerless",
			args:     []string{"providers", "route-interactive"},
			wantName: "route-models-providerless",
			wantTier: tierProviderless,
		},
		{
			name:     "eval output routes providerless eval",
			args:     []string{"eval", "output", "actual.txt"},
			wantName: "eval-output",
			wantTier: tierProviderless,
		},
		{
			name:     "session show routes stateful session reader",
			args:     []string{"session", "show", "demo"},
			wantName: "session-read",
			wantTier: tierStateful,
		},
		{
			name:     "eval list routes stateful session reader",
			args:     []string{"eval", testCommandList},
			wantName: "session-read",
			wantTier: tierStateful,
		},
		{
			name:     "agents feedback apply routes stateful writer",
			args:     []string{"agents", "feedback-apply", "agents.yaml"},
			wantName: "session-write",
			wantTier: tierStateful,
		},
		{
			name:     "agents bash routes stateful local execution",
			args:     []string{"agents", "bash", "echo", "hello"},
			wantName: "bash-command",
			wantTier: tierStateful,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			opts := parseGroupedOptionsForRouteTest(t, tt.args)
			got, ok := firstMatchingCommand(opts)
			require.True(t, ok, "grouped command %#v should reach a registry handler", tt.args)
			assert.Equal(t, tt.wantName, got.name)
			assert.Equal(t, tt.wantTier, got.tier)
		})
	}
}

func TestCommandRegistry_StatefulSessionDispatcherPreservesAliases(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		wantName  string
		args      []string
		matchRead bool
	}{
		{
			name:      "session show dispatches to show-session alias",
			args:      []string{"session", "show", "demo"},
			matchRead: true,
			wantName:  "show-session",
		},
		{
			name:      "eval list dispatches to list-evaluations alias",
			args:      []string{"eval", testCommandList},
			matchRead: true,
			wantName:  "list-evaluations",
		},
		{
			name:     "session record-failure dispatches to record-failure alias",
			args:     []string{"session", "record-failure", "bad", "attempt"},
			wantName: "record-failure",
		},
		{
			name:     "agents feedback apply dispatches to feedback-apply alias",
			args:     []string{"agents", "feedback-apply", "agents.yaml"},
			wantName: "feedback-apply",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			opts := parseGroupedOptionsForRouteTest(t, tt.args)

			var got *statefulSessionCommand
			if tt.matchRead {
				got = matchingStatefulSessionReadCommand(opts)
			} else {
				got = matchingStatefulSessionWriteCommand(opts)
			}

			require.NotNil(t, got, "grouped command %#v should reach the session subdispatcher", tt.args)
			assert.Equal(t, tt.wantName, got.name)
		})
	}
}

func TestCommandRegistry_GroupedInlineCommandsBypassRegistry(t *testing.T) {
	t.Parallel()

	assertInlineGroupedRoute(t, []string{"config", "paths"}, func(t *testing.T, opts cliOptions) {
		t.Helper()
		assert.True(t, opts.listConfigPaths)
	})
	assertInlineGroupedRoute(t, []string{"config", testCommandTemplate}, func(t *testing.T, opts cliOptions) {
		t.Helper()
		assert.True(t, opts.printConfigTemplate)
	})
	assertInlineGroupedRoute(t, []string{"config", "init", "atteler.yaml"}, func(t *testing.T, opts cliOptions) {
		t.Helper()
		assert.Equal(t, "atteler.yaml", opts.initConfigPath)
	})
	assertInlineGroupedRoute(t, []string{"config", "validate"}, func(t *testing.T, opts cliOptions) {
		t.Helper()
		assert.True(t, opts.validateConfig)
	})
	assertInlineGroupedRoute(t, []string{"config", "explain", "default_model"}, func(t *testing.T, opts cliOptions) {
		t.Helper()
		assert.True(t, opts.explainConfig)
		assert.Equal(t, "default_model", opts.explainConfigPath)
		assert.Empty(t, opts.oncePrompt)
	})
	assertInlineGroupedRoute(t, []string{"config", testCommandVersion}, func(t *testing.T, opts cliOptions) {
		t.Helper()
		assert.True(t, opts.showVersion)
	})
	assertInlineGroupedRoute(t, []string{"providers", testCommandList}, func(t *testing.T, opts cliOptions) {
		t.Helper()
		assert.True(t, opts.listProviders)
	})
	assertInlineGroupedRoute(t, []string{"providers", "known-models"}, func(t *testing.T, opts cliOptions) {
		t.Helper()
		assert.True(t, opts.listKnownModels)
	})
	assertInlineGroupedRoute(t, []string{"worktrees", testCommandList}, func(t *testing.T, opts cliOptions) {
		t.Helper()
		assert.True(t, opts.listWorktrees)
	})
	assertInlineGroupedRoute(t, []string{"worktrees", "merge", "session-123"}, func(t *testing.T, opts cliOptions) {
		t.Helper()
		assert.Equal(t, "session-123", opts.mergeWorktreeRef)
	})
}

func TestApplyPositionalOptions_ConfigExplainOwnsPositionalFilter(t *testing.T) {
	t.Parallel()

	opts := cliOptions{explainConfig: true}
	applyPositionalOptions(&opts, []string{"providers.openai"})

	assert.True(t, opts.explainConfig)
	assert.Equal(t, "providers.openai", opts.explainConfigPath)
	assert.Empty(t, opts.oncePrompt)
}

func TestApplyPositionalOptions_ConfigExplainFieldEnablesExplain(t *testing.T) {
	t.Parallel()

	opts := cliOptions{explainConfigPath: "providers.openai"}
	applyPositionalOptions(&opts, []string{"ignored"})

	assert.True(t, opts.explainConfig)
	assert.Equal(t, "providers.openai", opts.explainConfigPath)
	assert.Empty(t, opts.oncePrompt)
}

func TestCommandRegistry_GroupedPromptCommandsBypassRegistry(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		wantPrompt string
		args       []string
		wantStdin  bool
	}{
		{
			name:       "chat once routes to one-shot prompt",
			args:       []string{"chat", "once", "explain", "this", "repo"},
			wantPrompt: "explain this repo",
		},
		{
			name:      "chat once stdin routes to one-shot stdin",
			args:      []string{"chat", "once", "--stdin"},
			wantStdin: true,
		},
		{
			name:       "chat run with prompt routes to positional one-shot prompt",
			args:       []string{"chat", "run", "explain", "this", "repo"},
			wantPrompt: "explain this repo",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			opts := parseGroupedOptionsForRouteTest(t, tt.args)
			_, ok := firstMatchingCommand(opts)
			assert.False(t, ok, "grouped prompt command %#v should be handled by runWithState, not the command registry", tt.args)
			assert.Equal(t, tt.wantPrompt, opts.oncePrompt)
			assert.Equal(t, tt.wantStdin, opts.readStdin)
		})
	}
}

func TestCommandRegistry_AllDocumentedCommandsReachDispatchPath(t *testing.T) {
	t.Parallel()

	for _, domain := range cliHelpDomains {
		t.Run(domain.Name, func(t *testing.T) {
			t.Parallel()

			for _, documentedCommand := range domain.Commands {
				command := documentedCommand
				t.Run(command.Name, func(t *testing.T) {
					t.Parallel()

					assertDocumentedDispatchPathForTest(t, domain, command, domainTokenForTest(domain), command.Name)
				})
			}
		})
	}
}

func TestCommandRegistry_AllDocumentedAliasesReachDispatchPath(t *testing.T) {
	t.Parallel()

	for _, domain := range cliHelpDomains {
		domainTokens := append([]string{domain.Name}, domain.Aliases...)
		for _, domainToken := range domainTokens {
			t.Run(domain.Name+"/"+domainToken, func(t *testing.T) {
				t.Parallel()

				for _, documentedCommand := range domain.Commands {
					command := documentedCommand

					commandTokens := append([]string{command.Name}, command.Aliases...)
					for _, commandToken := range commandTokens {
						t.Run(command.Name+"/"+commandToken, func(t *testing.T) {
							t.Parallel()

							assertDocumentedDispatchPathForTest(t, domain, command, domainToken, commandToken)
						})
					}
				}
			})
		}
	}
}

func TestCommandRegistry_GroupedCommandsWithSupplementalFlagsReachExpectedHandlers(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		wantName string
		args     []string
		wantTier commandTier
	}{
		{
			name:     "providers route-batch requires candidates",
			args:     []string{"providers", "route-batch", "--route-candidate", "openai/gpt-budget,input=0.001,output=0.001,max=1000"},
			wantName: "route-models-providerless",
			wantTier: tierProviderless,
		},
		{
			name:     "plugins mcp-tool requires manifest",
			args:     []string{"plugins", "mcp-tool", "lookup", "--mcp-manifest", "mcp.yaml"},
			wantName: "mcp-invoke",
			wantTier: tierProviderless,
		},
		{
			name:     "plugins mcp-method requires manifest",
			args:     []string{"plugins", "mcp-method", "ping", "--mcp-manifest", "mcp.yaml"},
			wantName: "mcp-invoke",
			wantTier: tierProviderless,
		},
		{
			name:     "review run routes stateful when executed",
			args:     []string{"review", "run"},
			wantName: "review-run",
			wantTier: tierStateful,
		},
		{
			name:     "agents async-run routes stateful when executed",
			args:     []string{"agents", "async-run"},
			wantName: "async-run",
			wantTier: tierStateful,
		},
		{
			name:     "agents speculate-run routes stateful when executed",
			args:     []string{"agents", "speculate-run"},
			wantName: "speculate-run",
			wantTier: tierStateful,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			opts := parseGroupedOptionsForRouteTest(t, tt.args)
			got, ok := firstMatchingCommand(opts)
			require.True(t, ok, "grouped command %#v should reach a registry handler", tt.args)
			assert.Equal(t, tt.wantName, got.name)
			assert.Equal(t, tt.wantTier, got.tier)
		})
	}
}

func TestCLIModularization_KeepsFormerMonolithFilesSmall(t *testing.T) {
	t.Parallel()

	tests := []struct {
		path     string
		maxLines int
	}{
		{path: "main.go", maxLines: 1000},
		{path: "codeintel_commands.go", maxLines: 500},
		{path: "command_registry.go", maxLines: 300},
		{path: "main_test.go", maxLines: 2500},
	}

	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			t.Parallel()

			lines := countFileLines(t, tt.path)
			assert.LessOrEqual(t, lines, tt.maxLines, "%s should stay split across focused CLI modules", tt.path)
		})
	}
}

func parseGroupedOptionsForRouteTest(t *testing.T, args []string) cliOptions {
	t.Helper()

	opts, fs := newCLIOptionsAndFlagSetForTest(t)
	plan := translateCLIArgsWithFlagSet(args, fs)
	require.NoError(t, plan.Err)
	require.False(t, plan.Help)
	require.NoError(t, fs.Parse(plan.Args), "translated args should parse: %#v", plan.Args)

	applyPositionalOptions(opts, fs.Args())

	return *opts
}

func assertInlineGroupedRoute(t *testing.T, args []string, check func(*testing.T, cliOptions)) {
	t.Helper()

	opts := parseGroupedOptionsForRouteTest(t, args)
	_, ok := firstMatchingCommand(opts)
	assert.False(t, ok, "grouped inline command %#v should be handled by runInlineCommand before the registry", args)
	check(t, opts)
}

func firstMatchingCommand(opts cliOptions) (command, bool) {
	for i := range commandRegistry {
		cmd := commandRegistry[i]
		if cmd.match(opts) {
			return cmd, true
		}
	}

	return command{}, false
}

func assertCommandBefore(t *testing.T, registry []command, beforeName, afterName string) {
	t.Helper()

	beforeIndex := commandIndex(registry, beforeName)
	afterIndex := commandIndex(registry, afterName)

	require.NotEqual(t, -1, beforeIndex, "missing command %q", beforeName)
	require.NotEqual(t, -1, afterIndex, "missing command %q", afterName)
	assert.Less(t, beforeIndex, afterIndex, "command %q should be registered before %q", beforeName, afterName)
}

func commandIndex(registry []command, name string) int {
	for index, command := range registry {
		if command.name == name {
			return index
		}
	}

	return -1
}

func countFileLines(t *testing.T, path string) int {
	t.Helper()

	data, err := os.ReadFile(path)
	require.NoError(t, err)

	text := strings.TrimRight(string(data), "\n")
	if text == "" {
		return 0
	}

	return len(strings.Split(text, "\n"))
}

func documentedDispatchArgsForTokens(
	domain cliHelpDomain,
	domainToken string,
	commandToken string,
	command cliCommandAlias,
) []string {
	args := documentedCommandArgsForTokens(domainToken, commandToken, command)

	switch {
	case domain.Name == testDomainProviders && (command.Name == "route-interactive" || command.Name == "route-batch"):
		args = append(args, "--route-candidate", "openai/gpt-fast,input=0.001,output=0.002,max=1000")
	case domain.Name == testDomainPlugins && (command.Name == "mcp-tool" || command.Name == "mcp-method"):
		args = append(args, "--mcp-manifest", "mcp.yaml")
	}

	return args
}

func assertDocumentedDispatchPathForTest(
	t *testing.T,
	domain cliHelpDomain,
	command cliCommandAlias,
	domainToken string,
	commandToken string,
) {
	t.Helper()

	opts := parseGroupedOptionsForRouteTest(t, documentedDispatchArgsForTokens(domain, domainToken, commandToken, command))
	got, ok := firstMatchingCommand(opts)

	switch documentedDispatchPathForTest(domain, command) {
	case "inline":
		assert.False(t, ok, "%s %s should be handled before the command registry", domainToken, commandToken)
		assertInlineOptionSetForTest(t, domain, command, opts)
	case "prompt":
		assert.False(t, ok, "%s %s should be handled by the prompt runner, not the command registry", domainToken, commandToken)
		assert.True(t, opts.oncePrompt != "" || opts.readStdin, "%s %s should set prompt execution options", domainToken, commandToken)
	default:
		require.True(t, ok, "%s %s should reach a command registry handler", domainToken, commandToken)
		assert.NotEmpty(t, got.name)
	}
}

func documentedDispatchPathForTest(domain cliHelpDomain, command cliCommandAlias) string {
	if isDocumentedInlineCommandForTest(domain, command) {
		return "inline"
	}

	if isDocumentedPromptCommandForTest(domain, command) {
		return "prompt"
	}

	return "registry"
}

func isDocumentedInlineCommandForTest(domain cliHelpDomain, command cliCommandAlias) bool {
	switch domain.Name {
	case testDomainConfig:
		switch command.Name {
		case "paths", testCommandTemplate, "init", "validate", "explain", testCommandDoctorOffline, testCommandVersion:
			return true
		}
	case testDomainProviders:
		switch command.Name {
		case testCommandList, "known-models":
			return true
		}
	case testDomainWorktrees:
		switch command.Name {
		case testCommandList, "merge":
			return true
		}
	}

	return false
}

func isDocumentedPromptCommandForTest(domain cliHelpDomain, command cliCommandAlias) bool {
	switch domain.Name {
	case testDomainChatSession:
		switch command.Name {
		case testCommandRun, "once":
			return true
		}
	case testDomainWorktrees:
		return command.Name == testCommandRun
	case testDomainEval:
		switch command.Name {
		case "record-response", "replay-response":
			return true
		}
	}

	return false
}

func assertInlineOptionSetForTest(t *testing.T, domain cliHelpDomain, command cliCommandAlias, opts cliOptions) {
	t.Helper()

	switch {
	case domain.Name == testDomainConfig && command.Name == "paths":
		assert.True(t, opts.listConfigPaths)
	case domain.Name == testDomainConfig && command.Name == testCommandTemplate:
		assert.True(t, opts.printConfigTemplate)
	case domain.Name == testDomainConfig && command.Name == "init":
		assert.NotEmpty(t, opts.initConfigPath)
	case domain.Name == testDomainConfig && command.Name == "validate":
		assert.True(t, opts.validateConfig)
	case domain.Name == testDomainConfig && command.Name == "explain":
		assert.True(t, opts.explainConfig)
	case domain.Name == testDomainConfig && command.Name == testCommandDoctorOffline:
		assert.True(t, opts.doctorOffline)
	case domain.Name == testDomainConfig && command.Name == testCommandVersion:
		assert.True(t, opts.showVersion)
	case domain.Name == testDomainProviders && command.Name == testCommandList:
		assert.True(t, opts.listProviders)
	case domain.Name == testDomainProviders && command.Name == "known-models":
		assert.True(t, opts.listKnownModels)
	case domain.Name == testDomainWorktrees && command.Name == testCommandList:
		assert.True(t, opts.listWorktrees)
	case domain.Name == testDomainWorktrees && command.Name == "merge":
		assert.NotEmpty(t, opts.mergeWorktreeRef)
	default:
		require.Failf(t, "missing inline assertion", "add assertion for %s %s", domain.Name, command.Name)
	}
}
