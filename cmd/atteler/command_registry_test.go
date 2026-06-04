package main

import (
	"encoding/json"
	"flag"
	"os"
	"reflect"
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
	testDispatchPathPrompt   = "prompt"
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

func TestCommandRegistry_ContractsAreWellFormed(t *testing.T) {
	t.Parallel()

	registry := buildCommandRegistry()
	contracts := commandContractsByName()
	commandNames := commandNamesForTest(registry, buildInlineCommandRegistry())

	for _, command := range registry {
		t.Run(command.name, func(t *testing.T) {
			t.Parallel()

			_, contractDeclared := contracts[command.name]
			require.True(t, contractDeclared, "adding a registry command requires an explicit contract")
			assert.NotEmpty(t, command.contract.Summary)
			assert.NotEmpty(t, command.contract.InputType)
			assert.NotEmpty(t, command.contract.InputFlags)
			assert.NotEmpty(t, command.contract.ConflictRules)
			assert.NotEmpty(t, command.contract.Examples)
			assert.NotEmpty(t, command.contract.SideEffects)
			assert.NotEmpty(t, command.contract.OutputModes)
			assert.NotEmpty(t, command.contract.Fixtures)
			assertKnownConflictRules(t, command.contract.ConflictRules)
			assertKnownContractValues(t, command.contract.SideEffects, knownSideEffectsForTest(), "side effect")
			assertKnownContractValues(t, command.contract.OutputModes, knownOutputModesForTest(), "output mode")

			for _, overrideName := range command.contract.Overrides {
				assert.True(t, commandNames[overrideName], "override target %q should name a registered command", overrideName)
			}

			for _, fixture := range command.contract.Fixtures {
				assert.Equal(t, command.name, fixture.WantCommand)
				assert.NotEmpty(t, fixture.Name)
				assertCommandFixtureSelectsCommand(t, fixture)
			}
		})
	}
}

func TestCommandRegistry_ContractValidatorRejectsMissingRequiredMetadata(t *testing.T) {
	t.Parallel()

	valid := commandContractFor(
		"valid contract",
		[]string{"--list-sessions"},
		[]string{commandEffectSessionRead},
		[]string{commandOutputText},
		withInputType("listSessionsCommandInput"),
	)
	valid.fillDerivedFields("list-sessions")

	tests := []struct {
		name string
		edit func(*commandContract)
		want string
	}{
		{
			name: "generic input type",
			edit: func(contract *commandContract) { contract.InputType = genericCLIOptionsInputType },
			want: "command-specific input type",
		},
		{
			name: "unknown input type",
			edit: func(contract *commandContract) { contract.InputType = "missingCommandInput" },
			want: "unknown input type",
		},
		{
			name: "missing side effects",
			edit: func(contract *commandContract) { contract.SideEffects = nil },
			want: "missing side effects",
		},
		{
			name: "missing output modes",
			edit: func(contract *commandContract) { contract.OutputModes = nil },
			want: "missing output modes",
		},
		{
			name: "unknown side effect",
			edit: func(contract *commandContract) { contract.SideEffects = []string{"database-write"} },
			want: "unknown side effect",
		},
		{
			name: "unknown output mode",
			edit: func(contract *commandContract) { contract.OutputModes = []string{"html"} },
			want: "unknown output mode",
		},
		{
			name: "missing conflict reason",
			edit: func(contract *commandContract) { contract.ConflictRules[0].Reason = "" },
			want: "missing reason",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			contract := cloneCommandContractForTest(valid)
			tt.edit(&contract)

			err := validateCommandContract("list-sessions", contract)
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.want)
		})
	}
}

func TestCommandRegistry_ContractValidatorRejectsBrokenRegistryMetadata(t *testing.T) {
	t.Parallel()

	baseCommand := buildCommandRegistry()[0]

	t.Run("duplicate command name", func(t *testing.T) {
		t.Parallel()

		err := validateCommandContracts([]command{baseCommand, baseCommand})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "duplicate command registry entry")
	})

	t.Run("unknown override target", func(t *testing.T) {
		t.Parallel()

		broken := baseCommand
		broken.contract.Overrides = []string{"missing-command"}

		err := validateCommandContracts([]command{broken})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "override target")
	})
}

func TestCommandRegistry_ContractInputTypesHaveBuilders(t *testing.T) {
	t.Parallel()

	builders := commandInputBuildersByType()
	seenTypes := make(map[string]bool)

	for _, command := range buildCommandSurface(buildCommandRegistry()).Commands {
		inputType := command.InputType
		if inputType == genericCLIOptionsInputType {
			require.Failf(t, "generic input type", "command %q should expose a command-specific input contract", command.Name)
		}

		seenTypes[inputType] = true

		builder, ok := builders[inputType]
		require.True(t, ok, "contract input type %q should have a command input builder", inputType)

		input := builder(cliOptions{})
		gotType := reflect.TypeOf(input)
		require.NotNil(t, gotType, "input builder %q should return a typed value", inputType)
		assert.Equal(t, inputType, gotType.Name())
	}

	for inputType := range builders {
		assert.True(t, seenTypes[inputType], "input builder %q should be referenced by a command contract", inputType)
	}
}

func TestCommandRegistry_ContractFlagsAreRegistered(t *testing.T) {
	t.Parallel()

	fs := newRegisteredFlagSetForTest(t)
	surface := buildCommandSurface(buildCommandRegistry())

	for _, command := range surface.Commands {
		t.Run("command/"+command.Name, func(t *testing.T) {
			t.Parallel()

			assertRegisteredContractFlagReferences(t, fs, commandContractFlagReferences(command))
		})
	}

	for _, domain := range surface.Domains {
		t.Run("domain/"+domain.Name, func(t *testing.T) {
			t.Parallel()

			for _, command := range domain.Commands {
				assertRegisteredContractFlagReferences(t, fs, command.LegacyFlags)
			}
		})
	}
}

func TestCommandRegistry_InputFlagMetadataCoversTypedFields(t *testing.T) {
	t.Parallel()

	for _, command := range buildCommandSurface(buildCommandRegistry()).Commands {
		t.Run(command.Name, func(t *testing.T) {
			t.Parallel()

			concreteInputFlags := concreteContractFlagReferences(command.InputFlags)
			assert.GreaterOrEqual(t, len(concreteInputFlags), len(command.InputFields),
				"input flags should cover typed input fields for %s", command.InputType)
		})
	}
}

func TestCommandRegistry_PrefixedCommandContractsCoverRegisteredFlags(t *testing.T) {
	t.Parallel()

	fs := newRegisteredFlagSetForTest(t)
	commands := commandSurfaceCommandsByName(buildCommandSurface(buildCommandRegistry()).Commands)

	require.Contains(t, commands, "code-intel")
	assert.ElementsMatch(t, append(registeredFlagsWithPrefix(fs, "code-"), "--json", "--output"), commands["code-intel"].InputFlags)

	require.Contains(t, commands, "lsp-symbols")
	assert.ElementsMatch(t, append(registeredFlagsWithPrefix(fs, "lsp-"), "--json", "--output"), commands["lsp-symbols"].InputFlags)
}

func TestCommandRegistry_InlineContractsAreWellFormed(t *testing.T) {
	t.Parallel()

	inlineCommands := buildInlineCommandRegistry()
	contracts := inlineCommandContractsByName()

	for _, command := range inlineCommands {
		t.Run(command.name, func(t *testing.T) {
			t.Parallel()

			_, contractDeclared := contracts[command.name]
			require.True(t, contractDeclared, "adding an inline command requires an explicit contract")
			assert.Equal(t, tierInline, command.tier)
			assert.NotEmpty(t, command.contract.Summary)
			assert.NotEmpty(t, command.contract.InputFlags)
			assert.NotEmpty(t, command.contract.ConflictRules)
			assert.NotEmpty(t, command.contract.SideEffects)
			assert.NotEmpty(t, command.contract.OutputModes)
			assertKnownConflictRules(t, command.contract.ConflictRules)
			assertKnownContractValues(t, command.contract.SideEffects, knownSideEffectsForTest(), "side effect")
			assertKnownContractValues(t, command.contract.OutputModes, knownOutputModesForTest(), "output mode")

			for _, fixture := range command.contract.Fixtures {
				assert.Equal(t, command.name, fixture.WantCommand)
				assertCommandFixtureSelectsCommand(t, fixture)
			}
		})
	}
}

func TestCommandRegistry_ContractFixturesAreUnambiguousOrExplicitlyOrdered(t *testing.T) {
	t.Parallel()

	commands := append([]command{}, buildCommandRegistry()...)
	commands = append(commands, buildInlineCommandRegistry()...)

	for i := range commands {
		cmd := &commands[i]

		t.Run(cmd.name, func(t *testing.T) {
			t.Parallel()

			for j := range cmd.contract.Fixtures {
				fixture := cmd.contract.Fixtures[j]
				opts := commandFixtureOptionsForTest(t, fixture)
				matches := matchingRegistryCommands(commands, tierAny, opts)
				require.NotEmpty(t, matches, "contract fixture should match at least one command: %#v", fixture.Args)

				winner, err := resolveCommandAmbiguity(matches)
				require.NoError(t, err, "fixture ambiguity must be resolved by contract overrides: %#v", fixture.Args)
				require.NotNil(t, winner)
				assert.Equal(t, fixture.WantCommand, winner.name)

				if len(matches) > 1 {
					assertCommandOverridesAllMatchedCommands(t, winner, matches)
				}
			}
		})
	}
}

func TestCommandSurface_RepresentativeSideEffectsAndOutputsAreStable(t *testing.T) {
	t.Parallel()

	commands := commandSurfaceCommandsByName(buildCommandSurface(buildCommandRegistry()).Commands)

	tests := []struct {
		name        string
		sideEffects []string
		outputModes []string
	}{
		{
			name:        "command-surface-json",
			sideEffects: []string{commandEffectUserOutput},
			outputModes: []string{commandOutputJSON},
		},
		{
			name:        "list-sessions",
			sideEffects: []string{commandEffectSessionRead, commandEffectUserOutput},
			outputModes: []string{commandOutputText},
		},
		{
			name:        "headless-command",
			sideEffects: []string{commandEffectSessionRead, commandEffectSessionWrite, commandEffectUserOutput, commandEffectProcessExecute},
			outputModes: []string{commandOutputText},
		},
		{
			name:        "bash-command",
			sideEffects: []string{commandEffectProcessExecute, commandEffectSessionWrite, commandEffectUserOutput},
			outputModes: []string{commandOutputProcess, commandOutputText},
		},
		{
			name:        "mcp-invoke",
			sideEffects: []string{commandEffectFilesystemRead, commandEffectProcessExecute, commandEffectUserOutput},
			outputModes: []string{commandOutputJSON, commandOutputText},
		},
		{
			name:        "session-read",
			sideEffects: []string{commandEffectSessionRead, commandEffectUserOutput},
			outputModes: []string{commandOutputText, commandOutputYAML, commandOutputMarkdown, commandOutputJSON},
		},
		{
			name:        "session-write",
			sideEffects: []string{commandEffectFilesystemWrite, commandEffectSessionRead, commandEffectSessionWrite, commandEffectUserOutput},
			outputModes: []string{commandOutputText},
		},
		{
			name: "prompt-complete-providerless",
			sideEffects: []string{
				commandEffectConfigRead,
				commandEffectFilesystemRead,
				commandEffectFilesystemWrite,
				commandEffectGitRead,
				commandEffectSessionRead,
				commandEffectUserOutput,
			},
			outputModes: []string{commandOutputText},
		},
		{
			name:        "watch-scan-providerless",
			sideEffects: []string{commandEffectFilesystemRead, commandEffectUserOutput},
			outputModes: []string{commandOutputText, commandOutputJSON},
		},
		{
			name: "retrieval-search",
			sideEffects: []string{
				commandEffectConfigRead,
				commandEffectFilesystemRead,
				commandEffectFilesystemWrite,
				commandEffectGitRead,
				commandEffectSessionRead,
				commandEffectUserOutput,
			},
			outputModes: []string{commandOutputText},
		},
		{
			name: "doctor-offline",
			sideEffects: []string{
				commandEffectConfigRead,
				commandEffectFilesystemRead,
				commandEffectStateRead,
				commandEffectUserOutput,
			},
			outputModes: []string{commandOutputText},
		},
		{
			name: "doctor",
			sideEffects: []string{
				commandEffectConfigRead,
				commandEffectFilesystemRead,
				commandEffectLLMProviderRead,
				commandEffectStateRead,
				commandEffectUserOutput,
			},
			outputModes: []string{commandOutputText},
		},
		{
			name:        "explain-config",
			sideEffects: []string{commandEffectConfigRead, commandEffectStateRead, commandEffectUserOutput},
			outputModes: []string{commandOutputText},
		},
		{
			name: "config-migrate",
			sideEffects: []string{
				commandEffectConfigRead,
				commandEffectFilesystemRead,
				commandEffectFilesystemWrite,
				commandEffectStateRead,
				commandEffectStateWrite,
				commandEffectUserOutput,
			},
			outputModes: []string{commandOutputText},
		},
		{
			name: "config-report",
			sideEffects: []string{
				commandEffectConfigRead,
				commandEffectFilesystemRead,
				commandEffectStateRead,
				commandEffectUserOutput,
			},
			outputModes: []string{commandOutputYAML},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			command, ok := commands[tt.name]
			require.True(t, ok, "missing command %q", tt.name)
			assert.ElementsMatch(t, tt.sideEffects, command.SideEffects)
			assert.ElementsMatch(t, tt.outputModes, command.OutputModes)
		})
	}
}

func TestCommandRegistry_AmbiguousCommandsFailHelpfulError(t *testing.T) {
	t.Parallel()

	opts := cliOptions{
		listSessions: true,
		searchQuery:  "auth retry",
	}

	_, handled, err := selectRegistryCommand(commandRegistry, tierProviderless, opts)
	require.Error(t, err)
	assert.True(t, handled)
	assert.Contains(t, err.Error(), "ambiguous CLI command")
	assert.Contains(t, err.Error(), "list-sessions")
	assert.Contains(t, err.Error(), "--list-sessions")
	assert.Contains(t, err.Error(), "search-sessions")
	assert.Contains(t, err.Error(), "--search-sessions")
}

func TestCommandRegistry_InlineRegistryAmbiguityFailsHelpfulError(t *testing.T) {
	t.Parallel()

	opts := cliOptions{
		showVersion:  true,
		listSessions: true,
	}

	err := validateCLICommandSelection(opts)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "show-version")
	assert.Contains(t, err.Error(), "--version")
	assert.Contains(t, err.Error(), "list-sessions")
	assert.Contains(t, err.Error(), "--list-sessions")
}

func TestCommandRegistry_HeadlessIDRequiresHeadlessMode(t *testing.T) {
	t.Parallel()

	err := validateCLICommandSelection(cliOptions{headlessID: "run-known"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "--headless-id requires --headless")

	err = validateCLICommandSelection(cliOptions{headlessID: " "})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "--headless-id requires --headless")
}

func TestCommandRegistry_HeadlessPrivateLogRequiresHeadlessMode(t *testing.T) {
	t.Parallel()

	err := validateCLICommandSelection(cliOptions{headlessPrivateLog: true})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "--headless-private-log requires --headless")
}

func TestCommandRegistry_InlineAmbiguityFailsHelpfulError(t *testing.T) {
	t.Parallel()

	opts := cliOptions{
		showVersion:   true,
		listProviders: true,
	}

	err := validateCLICommandSelection(opts)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "show-version")
	assert.Contains(t, err.Error(), "list-providers")
}

func TestCommandRegistry_CodeIntelSubcommandAmbiguityFailsHelpfulError(t *testing.T) {
	t.Parallel()

	opts := cliOptions{
		codeSummary:   true,
		listCodeFiles: true,
	}

	err := validateCLICommandSelection(opts)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "ambiguous CLI command")
	assert.Contains(t, err.Error(), "code-intel")
	assert.Contains(t, err.Error(), "code-summary")
	assert.Contains(t, err.Error(), "list-code-files")
}

func TestCommandRegistry_StatefulSessionReadSubcommandAmbiguityFailsHelpfulError(t *testing.T) {
	t.Parallel()

	opts := cliOptions{
		showSessionRef:    "demo",
		summarySessionRef: "demo",
	}

	err := validateCLICommandSelection(opts)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "ambiguous CLI command")
	assert.Contains(t, err.Error(), "session read")
	assert.Contains(t, err.Error(), "show-session")
	assert.Contains(t, err.Error(), "summary-session")
}

func TestCommandRegistry_StatefulRunReadSubcommandAmbiguityFailsHelpfulError(t *testing.T) {
	t.Parallel()

	opts := cliOptions{
		showRunRef:   "latest",
		exportRunRef: "latest",
	}

	err := validateCLICommandSelection(opts)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "ambiguous CLI command")
	assert.Contains(t, err.Error(), "session read")
	assert.Contains(t, err.Error(), "show-run")
	assert.Contains(t, err.Error(), "export-run")
}

func TestCommandRegistry_StatefulSessionWriteSubcommandAmbiguityFailsHelpfulError(t *testing.T) {
	t.Parallel()

	opts := cliOptions{
		recordFailure:  "bad",
		recordArtifact: "artifact.txt",
	}

	err := validateCLICommandSelection(opts)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "ambiguous CLI command")
	assert.Contains(t, err.Error(), "session write")
	assert.Contains(t, err.Error(), "record-failure")
	assert.Contains(t, err.Error(), "record-artifact")
}

func TestCommandRegistry_ExplicitPrecedenceResolvesDeclaredOverlap(t *testing.T) {
	t.Parallel()

	opts := cliOptions{
		mcpManifestPath: "mcp.yaml",
		mcpServerName:   "local",
		mcpToolName:     "lookup",
	}

	got, handled, err := selectRegistryCommand(commandRegistry, tierProviderless, opts)
	require.NoError(t, err)
	require.True(t, handled)
	require.NotNil(t, got)
	assert.Equal(t, "mcp-invoke", got.name)
	assert.Contains(t, got.contract.Overrides, "mcp-manifest")
}

func TestCommandRegistry_OrderingDoesNotSilentlyShadowAmbiguousMatches(t *testing.T) {
	t.Parallel()

	registry := []command{
		{
			name:  "first-match",
			tier:  tierProviderless,
			match: func(cliOptions) bool { return true },
			contract: commandContract{
				InputFlags: []string{"--first"},
			},
		},
		{
			name:  "shadowed-match",
			tier:  tierProviderless,
			match: func(cliOptions) bool { return true },
			contract: commandContract{
				InputFlags: []string{"--second"},
			},
		},
	}

	_, handled, err := selectRegistryCommand(registry, tierProviderless, cliOptions{})
	require.Error(t, err)
	assert.True(t, handled)
	assert.Contains(t, err.Error(), "first-match")
	assert.Contains(t, err.Error(), "shadowed-match")
}

func TestCommandRegistry_ExplicitPrecedenceMustCoverEveryShadowedMatch(t *testing.T) {
	t.Parallel()

	registry := []command{
		{
			name:  "declared-winner",
			tier:  tierProviderless,
			match: func(cliOptions) bool { return true },
			contract: commandContract{
				InputFlags: []string{"--winner"},
				Overrides:  []string{"known-overlap"},
			},
		},
		{
			name:  "known-overlap",
			tier:  tierProviderless,
			match: func(cliOptions) bool { return true },
			contract: commandContract{
				InputFlags: []string{"--known"},
			},
		},
		{
			name:  "new-shadow",
			tier:  tierProviderless,
			match: func(cliOptions) bool { return true },
			contract: commandContract{
				InputFlags: []string{"--new"},
			},
		},
	}

	_, handled, err := selectRegistryCommand(registry, tierProviderless, cliOptions{})
	require.Error(t, err)
	assert.True(t, handled)
	assert.Contains(t, err.Error(), "declared-winner")
	assert.Contains(t, err.Error(), "new-shadow")
}

func TestCommandSurface_JSONDumpIncludesDispatchContract(t *testing.T) {
	t.Parallel()

	var buf strings.Builder

	require.NoError(t, printCommandSurfaceJSON(&buf))

	var surface commandSurface
	require.NoError(t, json.Unmarshal([]byte(buf.String()), &surface))
	assert.Equal(t, commandSurfaceSchema, surface.Schema)
	require.NotEmpty(t, surface.Domains)
	require.NotEmpty(t, surface.Commands)
	require.NotEmpty(t, surface.SlashCommands)

	domains := make(map[string]commandSurfaceDomain, len(surface.Domains))
	for _, domain := range surface.Domains {
		domains[domain.Name] = domain
	}

	commands := make(map[string]commandSurfaceCommand, len(surface.Commands))
	for _, command := range surface.Commands {
		commands[command.Name] = command
	}

	slashCommands := make(map[string]commandSurfaceSlashCommand, len(surface.SlashCommands))
	for _, command := range surface.SlashCommands {
		slashCommands[command.Name] = command
	}

	require.Contains(t, domains, "config")
	assert.Contains(t, commandSurfaceDomainCommandNames(domains["config"].Commands), "commands-json")
	assert.Contains(t, commandSurfaceDomainCommandNames(domains["config"].Commands), "commands-docs")
	assert.Contains(t, commandSurfaceDomainCommandNames(domains["code-intel"].RoutingCommands), "symbol-name-file-summary")
	assert.Contains(t, commands, "command-surface-json")
	assert.Contains(t, commands, "list-sessions")
	assert.Contains(t, commands, "headless-command")
	assert.Contains(t, commands, "mcp-invoke")
	assert.Equal(t, "providerless", commands["list-sessions"].Tier)
	assert.Equal(t, "listSessionsCommandInput", commands["list-sessions"].InputType)
	assert.Equal(t, "headlessCommandInput", commands["headless-command"].InputType)
	assert.Equal(t, "routeModelsCommandInput", commands["route-models-providerless"].InputType)
	assert.Contains(t, commands["route-models-providerless"].InputFlags, "--route-cache-write-tokens")
	assert.Equal(t, "bashCommandInput", commands["bash-command"].InputType)
	assert.Equal(t, "spawnAgentsCommandInput", commands["spawn-agents"].InputType)
	assert.Contains(t, commands["list-sessions"].InputFields, "Tag")
	assert.Contains(t, commands["headless-command"].InputFields, "CancelID")
	assert.Contains(t, commands["headless-command"].InputFields, "Recover")
	assert.Contains(t, commands["route-models-providerless"].InputFields, "Candidates")
	assert.Contains(t, commands["bash-command"].InputFields, "Command")
	assert.Contains(t, commands["spawn-agents"].InputFields, "Specs")
	assert.Contains(t, commands["mcp-invoke"].Overrides, "mcp-manifest")
	assert.Contains(t, commands["mcp-invoke"].OutputModes, commandOutputJSON)
	assert.Contains(t, commands["list-sessions"].SideEffects, commandEffectSessionRead)
	assert.Contains(t, commands["headless-command"].SideEffects, commandEffectProcessExecute)
	assert.Contains(t, commands["headless-command"].SideEffects, commandEffectSessionWrite)

	require.Contains(t, slashCommands, "apply-patch")
	assert.Equal(t, "/apply-patch", slashCommands["apply-patch"].Usage)
	assert.Contains(t, slashCommands["apply-patch"].SideEffects, commandEffectWorktreeWrite)
	assert.Contains(t, slashCommands["apply-patch"].PolicyRequirements, slashPolicyMutatesWorktree)
	require.Contains(t, slashCommands, "eval")
	assert.Contains(t, slashCommands["eval"].PolicyRequirements, slashPolicyMutatesFilesystem)
	require.Len(t, slashCommands["eval"].Variants, 2)
	assert.Equal(t, "/eval add", slashCommands["eval"].Variants[0].Usage)
	require.Contains(t, slashCommands, "tokens")
	assert.Equal(t, []string{"cost"}, slashCommands["tokens"].HelpAliases)
	assert.Equal(t, 1, slashCommands["tokens"].HelpGroup)
}

func TestCommandSurface_DomainCommandsLinkToDispatchContract(t *testing.T) {
	t.Parallel()

	surface := buildCommandSurface(commandRegistry)

	sessionList := requireDomainCommand(t, surface, "chat/session", "list")
	assert.Equal(t, []string{"list-sessions"}, sessionList.DispatchCommands)
	assert.Contains(t, sessionList.SideEffects, commandEffectSessionRead)
	assert.Contains(t, sessionList.OutputModes, commandOutputText)

	headlessStatus := requireDomainCommand(t, surface, "chat/session", "status-headless")
	assert.Equal(t, []string{"headless-command"}, headlessStatus.DispatchCommands)
	assert.Contains(t, headlessStatus.SideEffects, commandEffectSessionRead)

	headlessCancel := requireDomainCommand(t, surface, "chat/session", "cancel-headless")
	assert.Equal(t, []string{"headless-command"}, headlessCancel.DispatchCommands)
	assert.Contains(t, headlessCancel.SideEffects, commandEffectProcessExecute)
	assert.Contains(t, headlessCancel.SideEffects, commandEffectSessionWrite)

	mcpManifest := requireDomainCommand(t, surface, "plugins", "mcp-manifest")
	assert.Equal(t, []string{"mcp-manifest"}, mcpManifest.DispatchCommands)

	routeBatch := requireDomainCommand(t, surface, "providers", "route-batch")
	assert.Equal(t, []string{"route-models-providerless"}, routeBatch.DispatchCommands)

	lspWorkspace := requireDomainCommand(t, surface, "code-intel", "lsp-workspace")
	assert.Equal(t, []string{"lsp-symbols"}, lspWorkspace.DispatchCommands)

	symbolNameFileSummary := requireDomainRoutingCommand(t, surface, "code-intel", "symbol-name-file-summary")
	assert.Equal(t, []string{"code-intel"}, symbolNameFileSummary.DispatchCommands)
	assert.Contains(t, symbolNameFileSummary.OutputModes, commandOutputJSON)

	oneShotPrompt := requireDomainCommand(t, surface, "chat/session", "once")
	assert.Empty(t, oneShotPrompt.DispatchCommands, "one-shot prompt execution is intentionally outside the command registry")
}

func TestCommandSurface_DocumentedRegistryCommandsDeclareDispatchLinks(t *testing.T) {
	t.Parallel()

	surface := buildCommandSurface(commandRegistry)

	for domainIndex := range cliHelpDomains {
		domain := cliHelpDomains[domainIndex]

		t.Run(domain.Name, func(t *testing.T) {
			t.Parallel()

			for commandIndex := range domain.Commands {
				documentedCommand := domain.Commands[commandIndex]
				got := requireDomainCommand(t, surface, domain.Name, documentedCommand.Name)

				if documentedDispatchPathForTest(domain, documentedCommand) == testDispatchPathPrompt {
					assert.Empty(t, got.DispatchCommands, "%s %s should stay marked as prompt-owned", domain.Name, documentedCommand.Name)
					continue
				}

				assert.NotEmpty(t, got.DispatchCommands, "%s %s should link to dispatch command descriptors", domain.Name, documentedCommand.Name)
				assert.NotEmpty(t, got.SideEffects, "%s %s should inherit dispatch side effects", domain.Name, documentedCommand.Name)
				assert.NotEmpty(t, got.OutputModes, "%s %s should inherit dispatch output modes", domain.Name, documentedCommand.Name)
			}

			for commandIndex := range domain.RoutingCommands {
				routingCommand := domain.RoutingCommands[commandIndex]
				if documentedDomainCommandForTest(domain, routingCommand.Name) {
					continue
				}

				got := requireDomainRoutingCommand(t, surface, domain.Name, routingCommand.Name)
				assert.NotEmpty(t, got.DispatchCommands, "%s %s should link routing-only command descriptors", domain.Name, routingCommand.Name)
				assert.NotEmpty(t, got.SideEffects, "%s %s should inherit routing-only side effects", domain.Name, routingCommand.Name)
				assert.NotEmpty(t, got.OutputModes, "%s %s should inherit routing-only output modes", domain.Name, routingCommand.Name)
			}
		})
	}
}

func TestCommandSurface_MarkdownDocsRenderFromSurface(t *testing.T) {
	t.Parallel()

	docs := renderCommandSurfaceMarkdown(buildCommandSurface(commandRegistry))

	assert.Contains(t, docs, "# Atteler command surface")
	assert.Contains(t, docs, "Schema: `"+commandSurfaceSchema+"`")
	assert.Contains(t, docs, "## Chat & sessions")
	assert.Contains(t, docs, "Commands:")
	assert.Contains(t, docs, "Routing-only commands:")
	assert.Contains(t, docs, "`symbol-name-file-summary <name>`: list files with symbol counts for one exact name")
	assert.Contains(t, docs, "`commands-json`: dump the inspectable CLI command surface as JSON")
	assert.Contains(t, docs, "`list`: list saved sessions (dispatch: `list-sessions`)")
	assert.Contains(t, docs, `atteler session list`)
	assert.Contains(t, docs, "`list-sessions` (providerless)")
	assert.Contains(t, docs, "- Input: `listSessionsCommandInput`")
	assert.Contains(t, docs, "- Input fields: `Tag`")
	assert.Contains(t, docs, "- Flags: `--list-sessions`, `--list-sessions-tag`")
	assert.Contains(t, docs, "- Examples: `atteler session list`")
	assert.Contains(t, docs, "- Conflicts:")
	assert.Contains(t, docs, "`exclusive-command` with `*`")
	assert.Contains(t, docs, "- Side effects: `session-store-read`, `stdout`")
	assert.Contains(t, docs, "- Outputs: `text`")
	assert.Contains(t, docs, "`headless`: list active headless runs (dispatch: `headless-command`)")
	assert.Contains(t, docs, "`cancel-headless <id>`: cancel one live headless run (dispatch: `headless-command`)")
	assert.Contains(t, docs, "`headless-command` (providerless)")
	assert.Contains(t, docs, "- Input: `headlessCommandInput`")
	assert.Contains(t, docs, "- Input fields: `StatusID`, `CancelID`, `StreamID`, `Recover`, `List`")
	assert.Contains(t, docs, "- Flags: `--list-headless`, `--recover-headless`, `--status-headless`, `--cancel-headless`, `--stream-headless`")
	assert.Contains(t, docs, "- Side effects: `session-store-read`, `session-store-write`, `stdout`, `process-execute`")
	assert.Contains(t, docs, "- Fixtures:")
	assert.Contains(t, docs, "`legacy-flag`: `atteler --list-sessions` -> `list-sessions`")
	assert.Contains(t, docs, "`command-surface-json` (inline)")
	assert.Contains(t, docs, "`command-surface-docs` (inline)")
	assert.Contains(t, docs, "`bash-command` (stateful)")
	assert.Contains(t, docs, "- Input: `bashCommandInput`")
	assert.Contains(t, docs, "- Input fields: `Command`, `Dir`, `TimeoutSeconds`")
	assert.Contains(t, docs, "- Flags: `--bash`, `--bash-dir`, `--bash-timeout-seconds`")
	assert.Contains(t, docs, "`mcp-invoke` (providerless)")
	assert.Contains(t, docs, "- Overrides: `mcp-manifest`")
	assert.Contains(t, docs, "## Interactive slash commands")
	assert.Contains(t, docs, "`/apply-patch`: apply the last assistant unified diff with git apply")
	assert.Contains(t, docs, "`/save-code <n> <path>`: save a code block from the last assistant response")
	assert.Contains(t, docs, "- Input fields: `Path`, `Block`")
	assert.Contains(t, docs, "- Arguments: `n:int required`, `path:string required`")
	assert.Contains(t, docs, "- Arguments: `mode:enum(plan|execute) optional`")
	assert.Contains(t, docs, "- Completion tokens: `patch`")
	assert.Contains(t, docs, "- Completion tokens: `plan`, `execute`")
	assert.Contains(t, docs, "- `/eval add`: add the current session to local evaluation cases")
	assert.Contains(t, docs, "Policy: `mutates-filesystem`")
	assert.Contains(t, docs, "- Policy: `runs-local-process`, `mutates-worktree`")
	assert.Contains(t, docs, "`code-intel` (providerless-config)")
	assert.Contains(t, docs, "- Input fields: `SymbolName`, `SymbolFileSummary`")
	assert.Contains(t, docs, "`--code-limit`")
	assert.Contains(t, docs, "`--code-offset`")
}

func TestCommandSurface_MarkdownSlashHelpRendersFromSurface(t *testing.T) {
	t.Parallel()

	docs := renderCommandSurfaceMarkdown(commandSurface{
		Schema: commandSurfaceSchema,
		SlashCommands: []commandSurfaceSlashCommand{
			{
				Name:               "demo",
				Usage:              "/demo",
				Summary:            "demo command",
				InputType:          "slashNoArgsInput",
				SideEffects:        []string{commandEffectUserOutput},
				OutputModes:        []string{commandOutputText},
				PolicyRequirements: nil,
			},
			{
				Name:               "next",
				Usage:              "/next [value]",
				Summary:            "next command",
				InputType:          "slashOptionalValueInput",
				HelpAliases:        []string{"n"},
				CompletionTokens:   []string{"next-token"},
				SideEffects:        []string{commandEffectUserOutput},
				OutputModes:        []string{commandOutputText},
				PolicyRequirements: nil,
				HelpGroup:          1,
			},
		},
	})

	assert.Contains(t, docs, "```text\n/demo\n/next [value] /n\n```")
	assert.NotContains(t, docs, "/apply-patch", "markdown rendering should use the provided command surface, not global descriptors")
	assert.Contains(t, docs, "- Help aliases: `n`")
	assert.Contains(t, docs, "- Completion tokens: `next-token`")
}

func TestCommandSurface_MarkdownDocsCommandWritesContractDocs(t *testing.T) {
	t.Parallel()

	var out strings.Builder

	require.NoError(t, printCommandSurfaceMarkdown(&out))
	assert.Contains(t, out.String(), "`command-surface-docs` (inline)")
	assert.Contains(t, out.String(), "render command surface docs from the dispatch contract")
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
			name:     "session headless routes providerless lifecycle command",
			args:     []string{"session", "headless"},
			wantName: "headless-command",
			wantTier: tierProviderless,
		},
		{
			name:     "session status-headless routes providerless lifecycle command",
			args:     []string{"session", "status-headless", "run-123"},
			wantName: "headless-command",
			wantTier: tierProviderless,
		},
		{
			name:     "session cancel-headless routes providerless lifecycle command",
			args:     []string{"session", "cancel-headless", "run-123"},
			wantName: "headless-command",
			wantTier: tierProviderless,
		},
		{
			name:     "session recover-headless routes providerless lifecycle command",
			args:     []string{"session", "recover-headless"},
			wantName: "headless-command",
			wantTier: tierProviderless,
		},
		{
			name:     "session stream-headless routes providerless lifecycle command",
			args:     []string{"session", "stream-headless", "run-123"},
			wantName: "headless-command",
			wantTier: tierProviderless,
		},
		{
			name:     "legacy list-headless flag routes providerless lifecycle command",
			args:     []string{"--list-headless"},
			wantName: "headless-command",
			wantTier: tierProviderless,
		},
		{
			name:     "legacy status-headless flag routes providerless lifecycle command",
			args:     []string{"--status-headless", "run-123"},
			wantName: "headless-command",
			wantTier: tierProviderless,
		},
		{
			name:     "legacy cancel-headless flag routes providerless lifecycle command",
			args:     []string{"--cancel-headless", "run-123"},
			wantName: "headless-command",
			wantTier: tierProviderless,
		},
		{
			name:     "legacy recover-headless flag routes providerless lifecycle command",
			args:     []string{"--recover-headless"},
			wantName: "headless-command",
			wantTier: tierProviderless,
		},
		{
			name:     "legacy stream-headless flag routes providerless lifecycle command",
			args:     []string{"--stream-headless", "run-123"},
			wantName: "headless-command",
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
			name:     "config state routes providerless diagnostics",
			args:     []string{"config", "state"},
			wantName: "state-diagnostics",
			wantTier: tierProviderlessConfig,
		},
		{
			name:     "providers models routes stateful provider inventory",
			args:     []string{"providers", "models"},
			wantName: "list-models",
			wantTier: tierStateful,
		},
		{
			name:     "providers resolve routes stateful resolution diagnostics",
			args:     []string{"providers", "resolve", "gpt-test"},
			wantName: "explain-model-resolution",
			wantTier: tierStateful,
		},
		{
			name:     "memory search routes providerless",
			args:     []string{"memory", "search", "hello", "auth"},
			wantName: "memory-command",
			wantTier: tierProviderless,
		},
		{
			name:     "memory purge routes providerless",
			args:     []string{"memory", "purge", "session:demo", "--memory-store", "memory.json"},
			wantName: "memory-command",
			wantTier: tierProviderless,
		},
		{
			name:     "memory rebuild routes providerless",
			args:     []string{"memory", "rebuild", "--memory-store", "memory.json", "--memory-scope", "repo"},
			wantName: "memory-command",
			wantTier: tierProviderless,
		},
		{
			name:     "memory list corpus routes providerless",
			args:     []string{"memory", "list-corpus", "--memory-store", "memory.json"},
			wantName: "memory-command",
			wantTier: tierProviderless,
		},
		{
			name:     "memory retention routes providerless",
			args:     []string{"--memory-store", "memory.json", "--memory-retention-days", "30"},
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
			name:     "eval run routes providerless eval",
			args:     []string{"eval", "run", "suite.eval.yaml"},
			wantName: "eval-output",
			wantTier: tierProviderless,
		},
		{
			name:     "eval fixtures routes providerless eval",
			args:     []string{"eval", "fixtures", ".atteler/evals"},
			wantName: "eval-output",
			wantTier: tierProviderless,
		},
		{
			name:     "eval json without target routes to providerless eval validation",
			args:     []string{"--eval-json"},
			wantName: "eval-output",
			wantTier: tierProviderless,
		},
		{
			name:     "memory retrieve routes unified retrieval search",
			args:     []string{"memory", "--retrieval-source", "session", "--retrieval-filter", "default_model=gpt-review", "--retrieval-explain", "retrieve", "oauth callback"},
			wantName: "retrieval-search",
			wantTier: tierProviderlessConfig,
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
			name:     "agents feedback rollback routes providerless config",
			args:     []string{"agents", "--feedback-rollback-agent", "reviewer", "--feedback-rollback-id", "fg-1", "feedback-rollback", "agents.yaml"},
			wantName: "feedback-rollback",
			wantTier: tierProviderlessConfig,
		},
		{
			name:     "agents feedback approve routes providerless config",
			args:     []string{"agents", "--feedback-approve-agent", "reviewer", "--feedback-approve-id", "fg-1", "feedback-approve", "agents.yaml"},
			wantName: "feedback-approve",
			wantTier: tierProviderlessConfig,
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

func TestCommandRegistry_CodeIntelGroupedStructuredFlagsReachSchemaInput(t *testing.T) {
	t.Parallel()

	opts := parseGroupedOptionsForRouteTest(t, []string{
		"code-intel",
		"symbol",
		"Run",
		"--json",
		"--code-limit",
		"1",
		"--code-offset",
		"2",
	})

	got, ok := firstMatchingCommand(opts)
	require.True(t, ok)
	assert.Equal(t, codeIntelDomainName, got.name)
	assert.Equal(t, tierProviderlessConfig, got.tier)

	input := codeIntelCommandInputFromOptions(opts)
	selected, err := selectCodeIntelCommand(input)
	require.NoError(t, err)
	require.NotNil(t, selected)
	assert.Equal(t, "code-symbol-name", selected.name)
	assert.Equal(t, "Run", input.SymbolName)
	assert.True(t, input.JSON)
	assert.Equal(t, 1, input.Limit)
	assert.Equal(t, 2, input.Offset)
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
			name:      "session runs dispatches to list-runs alias",
			args:      []string{"session", "runs"},
			matchRead: true,
			wantName:  "list-runs",
		},
		{
			name:      "session show-run dispatches to show-run alias",
			args:      []string{"session", "show-run", "latest"},
			matchRead: true,
			wantName:  "show-run",
		},
		{
			name:      "session export-run dispatches to export-run alias",
			args:      []string{"session", "export-run", "review"},
			matchRead: true,
			wantName:  "export-run",
		},
		{
			name:      "session replay-run dispatches to replay-run alias",
			args:      []string{"session", "replay-run", "speculation"},
			matchRead: true,
			wantName:  "replay-run",
		},
		{
			name:      "session resume-run dispatches to resume-run alias",
			args:      []string{"session", "resume-run", "latest"},
			matchRead: true,
			wantName:  "resume-run",
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

			var gotName string

			if tt.matchRead {
				got := matchingStatefulSessionReadCommand(sessionReadCommandInputFromOptions(opts))
				require.NotNil(t, got, "grouped command %#v should reach the session read subdispatcher", tt.args)
				gotName = got.name
			} else {
				got := matchingStatefulSessionWriteCommand(sessionWriteCommandInputFromOptions(opts))
				require.NotNil(t, got, "grouped command %#v should reach the session write subdispatcher", tt.args)
				gotName = got.name
			}

			assert.Equal(t, tt.wantName, gotName)
		})
	}
}

func TestCommandRegistry_SessionCommandInputsCopyOnlySessionFields(t *testing.T) {
	t.Parallel()

	opts := cliOptions{
		replayRef:             "replay-id",
		showSessionRef:        "show-id",
		summarySessionRef:     "summary-id",
		exportRef:             "export-id",
		exportFormat:          "json",
		showRunRef:            "run-show",
		exportRunRef:          "run-export",
		replayRunRef:          "run-replay",
		resumeRunRef:          "run-resume",
		listArtifacts:         true,
		listEvaluations:       true,
		listFailures:          true,
		listMessages:          true,
		listRuns:              true,
		recordFailure:         "bad-approach",
		failureReason:         "flaky",
		failureCommit:         "abc123",
		recordEvaluation:      "reviewer",
		evaluationOutcome:     "pass",
		evaluationNotes:       "clear",
		evaluationReference:   "run-1",
		evaluationReportPath:  "eval-report.json",
		recordArtifact:        "artifact.md",
		artifactKind:          "report",
		artifactLogicalPath:   "docs/decision.md",
		artifactReviewStatus:  "approved",
		artifactSummary:       "summary",
		feedbackApplyConfig:   "agents.yaml",
		feedbackHistoryPath:   "history.json",
		agentName:             "unrelated-agent-selection",
		searchQuery:           "unrelated providerless command",
		routeInteractive:      true,
		listProviders:         true,
		evaluationScore:       nonNegativeIntFlag{value: 3, set: true},
		evaluationPassRate:    floatFlag{value: 0, set: true},
		mergeArtifactMaxBytes: positiveIntFlag{value: 1024, set: true},
	}

	readInput := sessionReadCommandInputFromOptions(opts)
	assert.Equal(t, "replay-id", readInput.ReplayRef)
	assert.Equal(t, "show-id", readInput.ShowSessionRef)
	assert.Equal(t, "summary-id", readInput.SummarySessionRef)
	assert.Equal(t, "export-id", readInput.ExportRef)
	assert.Equal(t, "json", readInput.ExportFormat)
	assert.Equal(t, "run-show", readInput.ShowRunRef)
	assert.Equal(t, "run-export", readInput.ExportRunRef)
	assert.Equal(t, "run-replay", readInput.ReplayRunRef)
	assert.Equal(t, "run-resume", readInput.ResumeRunRef)
	assert.True(t, readInput.ListArtifacts)
	assert.True(t, readInput.ListEvaluations)
	assert.True(t, readInput.ListFailures)
	assert.True(t, readInput.ListMessages)
	assert.True(t, readInput.ListRuns)

	writeInput := sessionWriteCommandInputFromOptions(opts)
	assert.Equal(t, "bad-approach", writeInput.RecordFailure)
	assert.Equal(t, "flaky", writeInput.FailureReason)
	assert.Equal(t, "abc123", writeInput.FailureCommit)
	assert.Equal(t, "reviewer", writeInput.RecordEvaluation)
	assert.Equal(t, "pass", writeInput.EvaluationOutcome)
	assert.Equal(t, "clear", writeInput.EvaluationNotes)
	assert.Equal(t, "run-1", writeInput.EvaluationReference)
	assert.Equal(t, "eval-report.json", writeInput.EvaluationReportPath)
	assert.Equal(t, 3, writeInput.EvaluationScore)
	assert.True(t, writeInput.evaluationPassRateSet)
	assert.Zero(t, writeInput.EvaluationPassRate)
	assert.Equal(t, "artifact.md", writeInput.RecordArtifact)
	assert.Equal(t, "report", writeInput.ArtifactKind)
	assert.Equal(t, "docs/decision.md", writeInput.ArtifactLogicalPath)
	assert.Equal(t, "approved", writeInput.ArtifactReviewStatus)
	assert.Equal(t, "summary", writeInput.ArtifactSummary)
	assert.Equal(t, "agents.yaml", writeInput.FeedbackApplyConfig)
	assert.Equal(t, "history.json", writeInput.FeedbackHistoryPath)
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
	assertInlineGroupedRoute(t, []string{"config", "migrate"}, func(t *testing.T, opts cliOptions) {
		t.Helper()
		assert.True(t, opts.configMigrate)
	})
	assertInlineGroupedRoute(t, []string{"config", "report"}, func(t *testing.T, opts cliOptions) {
		t.Helper()
		assert.True(t, opts.configReport)
	})
	assertInlineGroupedRoute(t, []string{"config", "explain", "default_model"}, func(t *testing.T, opts cliOptions) {
		t.Helper()
		assert.True(t, opts.explainConfig)
		assert.Equal(t, "default_model", opts.explainConfigPath)
		assert.Empty(t, opts.oncePrompt)
	})
	assertInlineGroupedRoute(t, []string{"config", "commands-json"}, func(t *testing.T, opts cliOptions) {
		t.Helper()
		assert.True(t, opts.commandSurfaceJSON)
	})
	assertInlineGroupedRoute(t, []string{"config", "commands-docs"}, func(t *testing.T, opts cliOptions) {
		t.Helper()
		assert.True(t, opts.commandSurfaceDocs)
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
	assertInlineGroupedRoute(t, []string{"providers", commandOllamaStatus}, func(t *testing.T, opts cliOptions) {
		t.Helper()
		assert.True(t, opts.ollamaStatus)
	})
	assertInlineGroupedRoute(t, []string{"providers", commandOllamaStop}, func(t *testing.T, opts cliOptions) {
		t.Helper()
		assert.True(t, opts.ollamaStop)
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

func TestCommandRegistry_GroupedProviderResolveParsesModel(t *testing.T) {
	t.Parallel()

	opts := parseGroupedOptionsForRouteTest(t, []string{"providers", "resolve", "gpt-test"})
	got, ok := firstMatchingCommand(opts)
	require.True(t, ok)
	assert.Equal(t, "explain-model-resolution", got.name)
	assert.Equal(t, tierStateful, got.tier)
	assert.Equal(t, "gpt-test", opts.explainModelResolution)
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
			name:     "code-intel lsp-symbols routes to lsp providerless handler",
			args:     []string{"code-intel", "lsp-symbols", "--lsp-command", "gopls", "--lsp-file", "main.go"},
			wantName: codeIntelLSPSymbolsName,
			wantTier: tierProviderless,
		},
		{
			name:     "code-intel lsp-workspace routes to lsp providerless handler",
			args:     []string{"code-intel", "lsp-workspace", "Handler", "--lsp-command", "gopls"},
			wantName: codeIntelLSPSymbolsName,
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

func assertCommandFixtureSelectsCommand(t *testing.T, fixture commandFixture) {
	t.Helper()

	opts := commandFixtureOptionsForTest(t, fixture)

	got, handled, err := selectCLICommandForTest(opts)
	require.NoError(t, err, "contract fixture should not be ambiguous: %#v", fixture.Args)
	require.True(t, handled, "contract fixture should select a command: %#v", fixture.Args)
	require.NotNil(t, got)
	assert.Equal(t, fixture.WantCommand, got.name)
}

func commandFixtureOptionsForTest(t *testing.T, fixture commandFixture) cliOptions {
	t.Helper()

	opts, fs := newCLIOptionsAndFlagSetForTest(t)
	require.NoError(t, fs.Parse(fixture.Args), "contract fixture should parse: %#v", fixture.Args)
	applyPositionalOptions(opts, fs.Args())

	return *opts
}

func assertCommandOverridesAllMatchedCommands(t *testing.T, winner *command, matches []*command) {
	t.Helper()

	overrides := make(map[string]bool, len(winner.contract.Overrides))
	for _, name := range winner.contract.Overrides {
		overrides[name] = true
	}

	for _, match := range matches {
		if match.name == winner.name {
			continue
		}

		assert.True(t, overrides[match.name], "winner %q should explicitly override ambiguous match %q", winner.name, match.name)
	}
}

func cloneCommandContractForTest(contract commandContract) commandContract {
	contract.InputFlags = append([]string(nil), contract.InputFlags...)
	contract.ConflictRules = append([]commandConflictRule(nil), contract.ConflictRules...)
	contract.Examples = append([]string(nil), contract.Examples...)
	contract.SideEffects = append([]string(nil), contract.SideEffects...)
	contract.OutputModes = append([]string(nil), contract.OutputModes...)
	contract.Fixtures = append([]commandFixture(nil), contract.Fixtures...)
	contract.Overrides = append([]string(nil), contract.Overrides...)

	return contract
}

func assertKnownConflictRules(t *testing.T, rules []commandConflictRule) {
	t.Helper()

	knownKinds := map[string]bool{
		commandConflictExclusive: true,
		commandConflictOneOf:     true,
		commandConflictOrdered:   true,
	}

	for _, rule := range rules {
		assert.True(t, knownKinds[rule.Kind], "unknown conflict kind %q", rule.Kind)
		assert.NotEmpty(t, rule.Reason, "conflict rule %q should explain why it exists", rule.Kind)
	}
}

func assertKnownContractValues(t *testing.T, values []string, known map[string]bool, label string) {
	t.Helper()

	for _, value := range values {
		assert.True(t, known[value], "unknown %s %q", label, value)
	}
}

func commandNamesForTest(commandGroups ...[]command) map[string]bool {
	out := make(map[string]bool)

	for _, commands := range commandGroups {
		for i := range commands {
			out[commands[i].name] = true
		}
	}

	return out
}

func commandContractFlagReferences(command commandSurfaceCommand) []string {
	out := append([]string(nil), command.InputFlags...)

	for _, rule := range command.ConflictRules {
		out = append(out, rule.With...)
	}

	return out
}

func assertRegisteredContractFlagReferences(t *testing.T, fs *flag.FlagSet, refs []string) {
	t.Helper()

	for _, ref := range refs {
		if !isConcreteContractFlagReference(ref) {
			continue
		}

		assert.NotNil(t, fs.Lookup(strings.TrimPrefix(ref, "--")), "contract references unknown CLI flag %q", ref)
	}
}

func isConcreteContractFlagReference(ref string) bool {
	return strings.HasPrefix(ref, "--") && !strings.Contains(ref, "*")
}

func concreteContractFlagReferences(refs []string) []string {
	out := make([]string, 0, len(refs))

	for _, ref := range refs {
		if isConcreteContractFlagReference(ref) {
			out = append(out, ref)
		}
	}

	return out
}

func knownSideEffectsForTest() map[string]bool {
	return map[string]bool{
		commandEffectConfigRead:      true,
		commandEffectFilesystemRead:  true,
		commandEffectFilesystemWrite: true,
		commandEffectGitRead:         true,
		commandEffectLLMProviderRead: true,
		commandEffectProcessExecute:  true,
		commandEffectSessionRead:     true,
		commandEffectSessionWrite:    true,
		commandEffectStateRead:       true,
		commandEffectStateWrite:      true,
		commandEffectTaskWrite:       true,
		commandEffectUserOutput:      true,
		commandEffectWorktreeWrite:   true,
	}
}

func knownOutputModesForTest() map[string]bool {
	return map[string]bool{
		commandOutputFilesystem: true,
		commandOutputJSON:       true,
		commandOutputMarkdown:   true,
		commandOutputProcess:    true,
		commandOutputText:       true,
		commandOutputYAML:       true,
	}
}

func requireDomainCommand(
	t *testing.T,
	surface commandSurface,
	domainName string,
	commandName string,
) commandSurfaceDomainCommand {
	t.Helper()

	for i := range surface.Domains {
		if surface.Domains[i].Name != domainName {
			continue
		}

		for j := range surface.Domains[i].Commands {
			if surface.Domains[i].Commands[j].Name == commandName {
				return surface.Domains[i].Commands[j]
			}
		}

		require.Failf(t, "missing domain command", "domain %q command %q not found", domainName, commandName)

		return commandSurfaceDomainCommand{}
	}

	require.Failf(t, "missing domain", "domain %q not found", domainName)

	return commandSurfaceDomainCommand{}
}

func requireDomainRoutingCommand(
	t *testing.T,
	surface commandSurface,
	domainName string,
	commandName string,
) commandSurfaceDomainCommand {
	t.Helper()

	for i := range surface.Domains {
		if surface.Domains[i].Name != domainName {
			continue
		}

		for j := range surface.Domains[i].RoutingCommands {
			if surface.Domains[i].RoutingCommands[j].Name == commandName {
				return surface.Domains[i].RoutingCommands[j]
			}
		}

		require.Failf(t, "missing domain routing command", "domain %q routing command %q not found", domainName, commandName)

		return commandSurfaceDomainCommand{}
	}

	require.Failf(t, "missing domain", "domain %q not found", domainName)

	return commandSurfaceDomainCommand{}
}

func documentedDomainCommandForTest(domain cliHelpDomain, name string) bool {
	name = normalizeHelpName(name)

	for i := range domain.Commands {
		if normalizeHelpName(domain.Commands[i].Name) == name {
			return true
		}
	}

	return false
}

func commandSurfaceDomainCommandNames(commands []commandSurfaceDomainCommand) []string {
	names := make([]string, 0, len(commands))
	for i := range commands {
		names = append(names, commands[i].Name)
	}

	return names
}

func commandSurfaceCommandsByName(commands []commandSurfaceCommand) map[string]commandSurfaceCommand {
	out := make(map[string]commandSurfaceCommand, len(commands))

	for i := range commands {
		out[commands[i].Name] = commands[i]
	}

	return out
}

func registeredFlagsWithPrefix(fs *flag.FlagSet, prefix string) []string {
	var out []string

	fs.VisitAll(func(flag *flag.Flag) {
		if strings.HasPrefix(flag.Name, prefix) {
			out = append(out, "--"+flag.Name)
		}
	})

	return out
}

func selectCLICommandForTest(opts cliOptions) (*command, bool, error) {
	matches := matchingRegistryCommands(commandRegistry, tierAny, opts)
	inlineCommands := buildInlineCommandRegistry()

	matches = append(matches, matchingRegistryCommands(inlineCommands, tierInline, opts)...)
	if len(matches) == 0 {
		return nil, false, nil
	}

	winner, err := resolveCommandAmbiguity(matches)

	return winner, true, err
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
	for index := range registry {
		if registry[index].name == name {
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
	case commandTierInline:
		assert.False(t, ok, "%s %s should be handled before the command registry", domainToken, commandToken)
		assertInlineOptionSetForTest(t, domain, command, opts)
	case testDispatchPathPrompt:
		assert.False(t, ok, "%s %s should be handled by the prompt runner, not the command registry", domainToken, commandToken)
		assert.True(t, opts.oncePrompt != "" || opts.readStdin, "%s %s should set prompt execution options", domainToken, commandToken)
	default:
		require.True(t, ok, "%s %s should reach a command registry handler", domainToken, commandToken)
		assert.NotEmpty(t, got.name)
	}
}

func documentedDispatchPathForTest(domain cliHelpDomain, command cliCommandAlias) string {
	if isDocumentedInlineCommandForTest(domain, command) {
		return commandTierInline
	}

	if isDocumentedPromptCommandForTest(domain, command) {
		return testDispatchPathPrompt
	}

	return "registry"
}

func isDocumentedInlineCommandForTest(domain cliHelpDomain, command cliCommandAlias) bool {
	switch domain.Name {
	case testDomainConfig:
		switch command.Name {
		case "paths", testCommandTemplate, "init", "validate", "migrate", "report", "explain", "commands-json", "commands-docs", testCommandDoctorOffline, testCommandVersion:
			return true
		}
	case testDomainProviders:
		switch command.Name {
		case testCommandList, "known-models", commandOllamaStatus, commandOllamaStop:
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
	case domain.Name == testDomainConfig && command.Name == "migrate":
		assert.True(t, opts.configMigrate)
	case domain.Name == testDomainConfig && command.Name == "report":
		assert.True(t, opts.configReport)
	case domain.Name == testDomainConfig && command.Name == "explain":
		assert.True(t, opts.explainConfig)
	case domain.Name == testDomainConfig && command.Name == "commands-json":
		assert.True(t, opts.commandSurfaceJSON)
	case domain.Name == testDomainConfig && command.Name == "commands-docs":
		assert.True(t, opts.commandSurfaceDocs)
	case domain.Name == testDomainConfig && command.Name == testCommandDoctorOffline:
		assert.True(t, opts.doctorOffline)
	case domain.Name == testDomainConfig && command.Name == testCommandVersion:
		assert.True(t, opts.showVersion)
	case domain.Name == testDomainProviders && command.Name == testCommandList:
		assert.True(t, opts.listProviders)
	case domain.Name == testDomainProviders && command.Name == "known-models":
		assert.True(t, opts.listKnownModels)
	case domain.Name == testDomainProviders && command.Name == commandOllamaStatus:
		assert.True(t, opts.ollamaStatus)
	case domain.Name == testDomainProviders && command.Name == commandOllamaStop:
		assert.True(t, opts.ollamaStop)
	case domain.Name == testDomainWorktrees && command.Name == testCommandList:
		assert.True(t, opts.listWorktrees)
	case domain.Name == testDomainWorktrees && command.Name == "merge":
		assert.NotEmpty(t, opts.mergeWorktreeRef)
	default:
		require.Failf(t, "missing inline assertion", "add assertion for %s %s", domain.Name, command.Name)
	}
}
