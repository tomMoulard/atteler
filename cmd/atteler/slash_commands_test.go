package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tommoulard/atteler/pkg/autonomy"
	appconfig "github.com/tommoulard/atteler/pkg/config"
	"github.com/tommoulard/atteler/pkg/llm"
	"github.com/tommoulard/atteler/pkg/session"
)

func TestCopyToClipboard_RequiresActiveContext(t *testing.T) {
	t.Parallel()

	err := copyToClipboard(nil, "text") //nolint:staticcheck // Verify nil contexts are rejected instead of panicking.
	require.Error(t, err)
	assert.Contains(t, err.Error(), "context is required")

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err = copyToClipboard(ctx, "text")
	require.Error(t, err)
	require.ErrorIs(t, err, context.Canceled)
}

func TestSlashCommandAuditContextIncludesAutonomy(t *testing.T) {
	t.Parallel()

	got := slashCommandAuditContext(model{
		sessionState: session.Session{ID: "session-id"},
		sessionPath:  "/tmp/session.json",
		autonomy:     autonomy.High,
	}, "atteler.clipboard")

	assert.Equal(t, "atteler.clipboard", got.Caller)
	assert.Equal(t, "session-id", got.SessionID)
	assert.Equal(t, "/tmp/session.json", got.SessionPath)
	assert.Equal(t, "high", got.Autonomy)
}

func TestCopyToClipboardBlocksLowAutonomyBeforeShellExecution(t *testing.T) {
	t.Parallel()

	err := copyToClipboardWithAudit(context.Background(), "text", slashCommandAuditContext(model{autonomy: autonomy.Low}, "atteler.clipboard"))

	require.Error(t, err)
	assert.Contains(t, err.Error(), "autonomy low is advisory-only")
	assert.Contains(t, err.Error(), "--autonomy medium")
}

func TestSlashHelp_Golden(t *testing.T) {
	t.Parallel()

	want := `/help /model [name] /profile [name] /save /export [path] /clear /retry /edit /fork [n]
/tokens /cost /search <query> /pin <n> /unpin <n> /context [prune] /mode [plan|execute] /suggestions [status|local|session|folder|global]
/template [name] /codeblocks /save-code <n> <path> /copy [last|session] /copy-code [n] /apply-patch /eval add|run`

	assert.Equal(t, want, slashHelp())
}

func TestSlashHelp_MatchesCommandSurfaceHelp(t *testing.T) {
	t.Parallel()

	surface := buildCommandSurface(buildCommandRegistry())

	assert.Equal(t, slashHelp(), renderCommandSurfaceSlashHelp(surface.SlashCommands))
}

func TestSlashCommandDescriptors_AreTypedAndDeclarePolicy(t *testing.T) {
	t.Parallel()

	seen := map[string]bool{}
	knownPolicies := knownSlashPolicyRequirementsForTest()
	knownCommands := commandNamesForTest(buildCommandRegistry(), buildInlineCommandRegistry())
	commands := slashCommandDescriptors()

	for _, command := range commands {
		require.NotEmpty(t, command.Name)
		assert.False(t, seen[command.Name], "duplicate slash command %q", command.Name)
		seen[command.Name] = true

		got, ok := lookupSlashCommand(command.Name)
		require.True(t, ok, "canonical slash command %q should resolve", command.Name)
		assert.Equal(t, command.Name, got.Name)

		for _, alias := range command.Aliases {
			require.NotEmpty(t, alias)
			assert.False(t, seen[alias], "slash alias %q should not shadow another command or alias", alias)
			seen[alias] = true

			got, ok := lookupSlashCommand(alias)
			require.True(t, ok, "slash alias %q should resolve", alias)
			assert.Equal(t, command.Name, got.Name)
		}

		for _, alias := range command.HelpAliases {
			assert.Contains(t, command.Aliases, alias, "help alias %q should also be a parseable command alias", alias)
		}
	}

	for _, command := range commands {
		t.Run(command.Name, func(t *testing.T) {
			t.Parallel()

			require.NotEmpty(t, command.Name)
			require.NotEmpty(t, command.Usage)
			require.NotEmpty(t, command.Summary)
			require.NotEmpty(t, command.InputType)
			require.NotNil(t, command.Parse)
			require.NotNil(t, command.Run)

			assert.NotEmpty(t, command.SideEffects)
			assert.NotEmpty(t, command.OutputModes)
			assertKnownContractValues(t, command.SideEffects, knownSideEffectsForTest(), "slash side effect")
			assertKnownContractValues(t, command.OutputModes, knownOutputModesForTest(), "slash output mode")
			assertKnownContractValues(t, command.PolicyRequirements, knownPolicies, "slash policy")

			for _, argument := range command.Arguments {
				assert.NotEmpty(t, argument.Name)
				assert.NotEmpty(t, argument.Type)

				if argument.Type == slashArgumentTypeEnum {
					assert.NotEmpty(t, argument.Values)
				}
			}

			for _, variant := range command.Variants {
				assert.NotEmpty(t, variant.Name)
				assert.NotEmpty(t, variant.Usage)
				assert.NotEmpty(t, variant.Summary)
				assertKnownContractValues(t, variant.SideEffects, knownSideEffectsForTest(), "slash variant side effect")
				assertKnownContractValues(t, variant.OutputModes, knownOutputModesForTest(), "slash variant output mode")
				assertKnownContractValues(t, variant.PolicyRequirements, knownPolicies, "slash variant policy")
			}

			for _, shared := range command.SharedCLICommands {
				assert.True(t, knownCommands[shared], "shared CLI command %q should name a dispatch descriptor", shared)
			}

			if command.MutatesConversation || command.MutatesSessionStore || command.MutatesFilesystem || command.MutatesWorktree {
				assert.NotEmpty(t, command.PolicyRequirements, "mutating slash command should declare policy requirements")
			}
		})
	}
}

func TestSlashCommandValidation_UsesDescriptorSchemas(t *testing.T) {
	t.Parallel()

	tests := []struct {
		wantInput any
		name      string
		input     string
		wantName  string
		wantErr   string
	}{
		{
			name:      "alias resolves to descriptor",
			input:     "/regenerate",
			wantName:  "retry",
			wantInput: slashNoArgsInput{},
		},
		{
			name:      "search joins remaining args",
			input:     "/search auth retry",
			wantName:  "search",
			wantInput: slashSearchInput{Query: "auth retry"},
		},
		{
			name:      "quoted path stays one typed argument",
			input:     `/save-code 2 "notes/out file.go"`,
			wantName:  "save-code",
			wantInput: slashSaveCodeInput{Block: 2, Path: "notes/out file.go"},
		},
		{
			name:      "escaped space stays in typed argument",
			input:     `/export session\ transcript.md`,
			wantName:  "export",
			wantInput: slashOptionalValueInput{Value: "session transcript.md"},
		},
		{
			name:      "literal backslash before regular character is preserved",
			input:     `/export session\transcript.md`,
			wantName:  "export",
			wantInput: slashOptionalValueInput{Value: `session\transcript.md`},
		},
		{
			name:      "escaped quote stays inside quoted typed argument",
			input:     `/export "session \"draft\".md"`,
			wantName:  "export",
			wantInput: slashOptionalValueInput{Value: `session "draft".md`},
		},
		{
			name:      "single quotes stay one typed argument",
			input:     `/export 'session transcript.md'`,
			wantName:  "export",
			wantInput: slashOptionalValueInput{Value: "session transcript.md"},
		},
		{
			name:    "mode validates enum",
			input:   "/mode inspect",
			wantErr: "mode must be plan or execute",
		},
		{
			name:      "suggestions parses folder opt-in",
			input:     "/suggestions folder",
			wantName:  "suggestions",
			wantInput: slashSuggestionsInput{Mode: string(promptSuggestionConsentFolder)},
		},
		{
			name:      "suggestions parses no-network as local-only",
			input:     "/suggestions no-network",
			wantName:  "suggestions",
			wantInput: slashSuggestionsInput{Mode: string(promptSuggestionConsentLocalOnly)},
		},
		{
			name:    "suggestions validates enum",
			input:   "/suggestions always",
			wantErr: "usage: /suggestions [status|local|session|folder|global]",
		},
		{
			name:    "save-code requires block and path",
			input:   "/save-code 1",
			wantErr: "usage: /save-code <n> <path>",
		},
		{
			name:    "copy validates target",
			input:   "/copy clipboard",
			wantErr: "usage: /copy [last|session]",
		},
		{
			name:      "copy-code defaults to first block",
			input:     "/copy-code",
			wantName:  "copy-code",
			wantInput: slashCopyCodeInput{Block: 1},
		},
		{
			name:    "eval validates subcommand",
			input:   "/eval delete",
			wantErr: "usage: /eval add|run",
		},
		{
			name:    "unknown command reports help hint",
			input:   "/wat",
			wantErr: "unknown command: /wat (try /help)",
		},
		{
			name:    "unterminated quote reports parser error",
			input:   `/export "session.md`,
			wantErr: "unterminated quoted slash command argument",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			descriptor, parsed, handled, err := parseSlashCommandInput(tt.input)
			require.True(t, handled)

			if tt.wantErr != "" {
				require.Error(t, err)
				assert.Equal(t, tt.wantErr, err.Error())

				return
			}

			require.NoError(t, err)
			assert.Equal(t, tt.wantName, descriptor.Name)
			assert.Equal(t, tt.wantInput, parsed)
		})
	}
}

func TestSlashCommandParsers_AcceptDescriptorSchemaSamples(t *testing.T) {
	t.Parallel()

	for _, command := range slashCommandDescriptors() {
		t.Run(command.Name, func(t *testing.T) {
			t.Parallel()

			sampleArgs := slashSchemaSampleArgsForTest(command.Arguments)
			input := "/" + command.Name

			if len(sampleArgs) > 0 {
				input += " " + strings.Join(sampleArgs, " ")
			}

			descriptor, _, handled, err := parseSlashCommandInput(input)
			require.True(t, handled)
			require.NoError(t, err)
			assert.Equal(t, command.Name, descriptor.Name)

			if slashSchemaHasVariadicArgumentForTest(command.Arguments) {
				return
			}

			_, _, handled, err = parseSlashCommandInput(input + " unexpected")
			require.True(t, handled)
			require.Error(t, err, "non-variadic slash command %q should reject unexpected extra args", command.Name)
		})
	}
}

func TestSlashCommandDescriptorValidatorRejectsBrokenMetadata(t *testing.T) {
	t.Parallel()

	valid := typedSlashCommand(
		slashCommandDescriptor{
			Name:        "demo",
			Usage:       "/demo",
			Summary:     "demo command",
			SideEffects: []string{commandEffectUserOutput},
			OutputModes: []string{commandOutputText},
		},
		parseNoArgs,
		func(m model, _ slashNoArgsInput) (model, tea.Cmd, bool) {
			return m, nil, true
		},
	)

	tests := []struct {
		build func() []slashCommandDescriptor
		name  string
		want  string
	}{
		{
			name: "alias shadows command",
			build: func() []slashCommandDescriptor {
				return []slashCommandDescriptor{
					valid,
					typedSlashCommand(
						slashCommandDescriptor{
							Name:        "other",
							Usage:       "/other",
							Summary:     "other command",
							Aliases:     []string{"demo"},
							SideEffects: []string{commandEffectUserOutput},
							OutputModes: []string{commandOutputText},
						},
						parseNoArgs,
						func(m model, _ slashNoArgsInput) (model, tea.Cmd, bool) {
							return m, nil, true
						},
					),
				}
			},
			want: "shadows",
		},
		{
			name: "uppercase alias",
			build: func() []slashCommandDescriptor {
				return []slashCommandDescriptor{
					slashCommandDescriptorWithTestEdit(valid, func(command *slashCommandDescriptor) {
						command.Aliases = []string{"Demo"}
					}),
				}
			},
			want: "must be lowercase",
		},
		{
			name: "stale usage command name",
			build: func() []slashCommandDescriptor {
				return []slashCommandDescriptor{
					slashCommandDescriptorWithTestEdit(valid, func(command *slashCommandDescriptor) {
						command.Usage = "/other"
					}),
				}
			},
			want: "usage must start with /demo",
		},
		{
			name: "unknown shared cli command",
			build: func() []slashCommandDescriptor {
				return []slashCommandDescriptor{
					slashCommandDescriptorWithTestEdit(valid, func(command *slashCommandDescriptor) {
						command.SharedCLICommands = []string{"missing-command"}
					}),
				}
			},
			want: "unknown shared CLI command",
		},
		{
			name: "usage argument count mismatch",
			build: func() []slashCommandDescriptor {
				return []slashCommandDescriptor{
					slashCommandDescriptorWithTestEdit(valid, func(command *slashCommandDescriptor) {
						command.Arguments = []slashCommandArgument{requiredSlashArg("path", "string")}
					}),
				}
			},
			want: "usage argument count",
		},
		{
			name: "usage argument required marker mismatch",
			build: func() []slashCommandDescriptor {
				return []slashCommandDescriptor{
					slashCommandDescriptorWithTestEdit(valid, func(command *slashCommandDescriptor) {
						command.Usage = "/demo [path]"
						command.Arguments = []slashCommandArgument{requiredSlashArg("path", "string")}
					}),
				}
			},
			want: "required marker",
		},
		{
			name: "duplicate argument name",
			build: func() []slashCommandDescriptor {
				return []slashCommandDescriptor{
					slashCommandDescriptorWithTestEdit(valid, func(command *slashCommandDescriptor) {
						command.Usage = "/demo <path> <path>"
						command.Arguments = []slashCommandArgument{
							requiredSlashArg("path", "string"),
							requiredSlashArg("path", "string"),
						}
					}),
				}
			},
			want: "duplicate argument",
		},
		{
			name: "required argument after optional",
			build: func() []slashCommandDescriptor {
				return []slashCommandDescriptor{
					slashCommandDescriptorWithTestEdit(valid, func(command *slashCommandDescriptor) {
						command.Usage = "/demo [format] <path>"
						command.Arguments = []slashCommandArgument{
							optionalSlashArg("format", "string"),
							requiredSlashArg("path", "string"),
						}
					}),
				}
			},
			want: "required argument \"path\" follows an optional argument",
		},
		{
			name: "variadic argument before end",
			build: func() []slashCommandDescriptor {
				return []slashCommandDescriptor{
					slashCommandDescriptorWithTestEdit(valid, func(command *slashCommandDescriptor) {
						command.Usage = "/demo <query> <path>"
						command.Arguments = []slashCommandArgument{
							{Name: "query", Type: "string", Required: true, Variadic: true},
							requiredSlashArg("path", "string"),
						}
					}),
				}
			},
			want: "variadic argument \"query\" must be last",
		},
		{
			name: "usage enum values mismatch",
			build: func() []slashCommandDescriptor {
				return []slashCommandDescriptor{
					slashCommandDescriptorWithTestEdit(valid, func(command *slashCommandDescriptor) {
						command.Usage = "/demo read|write"
						command.Arguments = []slashCommandArgument{{Name: "action", Type: slashArgumentTypeEnum, Required: true, Values: []string{"add", "run"}}}
					}),
				}
			},
			want: "does not match schema argument",
		},
		{
			name: "enum values must be lowercase",
			build: func() []slashCommandDescriptor {
				return []slashCommandDescriptor{
					slashCommandDescriptorWithTestEdit(valid, func(command *slashCommandDescriptor) {
						command.Usage = "/demo Read"
						command.Arguments = []slashCommandArgument{{Name: "action", Type: slashArgumentTypeEnum, Required: true, Values: []string{"Read"}}}
					}),
				}
			},
			want: "must be lowercase",
		},
		{
			name: "enum values must be unique",
			build: func() []slashCommandDescriptor {
				return []slashCommandDescriptor{
					slashCommandDescriptorWithTestEdit(valid, func(command *slashCommandDescriptor) {
						command.Usage = "/demo read|read"
						command.Arguments = []slashCommandArgument{{Name: "action", Type: slashArgumentTypeEnum, Required: true, Values: []string{"read", "read"}}}
					}),
				}
			},
			want: "duplicate value",
		},
		{
			name: "enum values must be trimmed",
			build: func() []slashCommandDescriptor {
				return []slashCommandDescriptor{
					slashCommandDescriptorWithTestEdit(valid, func(command *slashCommandDescriptor) {
						command.Usage = "/demo read"
						command.Arguments = []slashCommandArgument{{Name: "action", Type: slashArgumentTypeEnum, Required: true, Values: []string{" read"}}}
					}),
				}
			},
			want: "must be trimmed",
		},
		{
			name: "unknown policy",
			build: func() []slashCommandDescriptor {
				return []slashCommandDescriptor{
					slashCommandDescriptorWithTestEdit(valid, func(command *slashCommandDescriptor) {
						command.PolicyRequirements = []string{"network-write"}
					}),
				}
			},
			want: "unknown policy requirement",
		},
		{
			name: "mutating command without policy",
			build: func() []slashCommandDescriptor {
				return []slashCommandDescriptor{
					slashCommandDescriptorWithTestEdit(valid, func(command *slashCommandDescriptor) {
						command.MutatesFilesystem = true
						command.PolicyRequirements = nil
					}),
				}
			},
			want: "without policy requirements",
		},
		{
			name: "write side effect without policy",
			build: func() []slashCommandDescriptor {
				return []slashCommandDescriptor{
					slashCommandDescriptorWithTestEdit(valid, func(command *slashCommandDescriptor) {
						command.SideEffects = []string{commandEffectFilesystemWrite}
						command.PolicyRequirements = nil
					}),
				}
			},
			want: "without policy requirements",
		},
		{
			name: "mutating variant without policy",
			build: func() []slashCommandDescriptor {
				return []slashCommandDescriptor{
					slashCommandDescriptorWithTestEdit(valid, func(command *slashCommandDescriptor) {
						command.Variants = []slashCommandVariant{
							{
								Name:        "write",
								Usage:       "/demo write",
								Summary:     "write demo output",
								SideEffects: []string{commandEffectFilesystemWrite},
								OutputModes: []string{commandOutputFilesystem},
							},
						}
						command.SideEffects = mergedSlashSideEffects(*command)
						command.OutputModes = mergedSlashOutputModes(*command)
					}),
				}
			},
			want: "variant \"write\" mutates state or crosses policy boundary without policy requirements",
		},
		{
			name: "variant usage must match variant",
			build: func() []slashCommandDescriptor {
				return []slashCommandDescriptor{
					slashCommandDescriptorWithTestEdit(valid, func(command *slashCommandDescriptor) {
						command.Variants = []slashCommandVariant{
							{
								Name:               "write",
								Usage:              "/demo read",
								Summary:            "write demo output",
								SideEffects:        []string{commandEffectFilesystemWrite},
								OutputModes:        []string{commandOutputFilesystem},
								PolicyRequirements: []string{slashPolicyMutatesFilesystem},
							},
						}
						command.SideEffects = mergedSlashSideEffects(*command)
						command.OutputModes = mergedSlashOutputModes(*command)
						command.PolicyRequirements = mergedSlashPolicyRequirements(*command)
					}),
				}
			},
			want: "usage must start with /demo write",
		},
		{
			name: "variant requires leading enum argument",
			build: func() []slashCommandDescriptor {
				return []slashCommandDescriptor{
					slashCommandDescriptorWithTestEdit(valid, func(command *slashCommandDescriptor) {
						command.Variants = []slashCommandVariant{
							{
								Name:        "read",
								Usage:       "/demo read",
								Summary:     "read demo output",
								SideEffects: []string{commandEffectUserOutput},
								OutputModes: []string{commandOutputText},
							},
						}
					}),
				}
			},
			want: "variants require a leading enum argument",
		},
		{
			name: "variant must be declared in enum argument",
			build: func() []slashCommandDescriptor {
				return []slashCommandDescriptor{
					slashCommandDescriptorWithTestEdit(valid, func(command *slashCommandDescriptor) {
						command.Usage = "/demo read"
						command.Arguments = []slashCommandArgument{{Name: "action", Type: slashArgumentTypeEnum, Required: true, Values: []string{"read"}}}
						command.Variants = []slashCommandVariant{
							{
								Name:        "write",
								Usage:       "/demo write",
								Summary:     "write demo output",
								SideEffects: []string{commandEffectUserOutput},
								OutputModes: []string{commandOutputText},
							},
						}
					}),
				}
			},
			want: "is not declared in enum argument",
		},
		{
			name: "enum value must have matching variant",
			build: func() []slashCommandDescriptor {
				return []slashCommandDescriptor{
					slashCommandDescriptorWithTestEdit(valid, func(command *slashCommandDescriptor) {
						command.Usage = "/demo read|write"
						command.Arguments = []slashCommandArgument{{Name: "action", Type: slashArgumentTypeEnum, Required: true, Values: []string{"read", "write"}}}
						command.Variants = []slashCommandVariant{
							{
								Name:        "read",
								Usage:       "/demo read",
								Summary:     "read demo output",
								SideEffects: []string{commandEffectUserOutput},
								OutputModes: []string{commandOutputText},
							},
						}
					}),
				}
			},
			want: "is missing a matching variant",
		},
		{
			name: "help groups must stay ordered",
			build: func() []slashCommandDescriptor {
				return []slashCommandDescriptor{
					slashCommandDescriptorWithTestEdit(valid, func(command *slashCommandDescriptor) {
						command.HelpGroup = 1
					}),
					typedSlashCommand(
						slashCommandDescriptor{
							Name:        "other",
							Usage:       "/other",
							Summary:     "other command",
							SideEffects: []string{commandEffectUserOutput},
							OutputModes: []string{commandOutputText},
							HelpGroup:   0,
						},
						parseNoArgs,
						func(m model, _ slashNoArgsInput) (model, tea.Cmd, bool) {
							return m, nil, true
						},
					),
				}
			},
			want: "appears after help group",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			err := validateSlashCommandDescriptors(tt.build())
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.want)
		})
	}
}

func TestSlashCommandSideEffects_AreStableForMutatingCommands(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		effects  []string
		policies []string
	}{
		{
			name:     "model",
			effects:  []string{commandEffectSessionWrite, commandEffectUserOutput},
			policies: []string{slashPolicyMutatesSessionStore},
		},
		{
			name:     "profile",
			effects:  []string{commandEffectConfigRead, commandEffectSessionWrite, commandEffectUserOutput},
			policies: []string{slashPolicyMutatesSessionStore},
		},
		{
			name:     "retry",
			effects:  []string{commandEffectLLMProviderRead, commandEffectSessionWrite, commandEffectUserOutput},
			policies: []string{slashPolicyMutatesConversation, slashPolicyMutatesSessionStore, slashPolicyCallsLLM},
		},
		{
			name:     "clear",
			effects:  []string{commandEffectSessionWrite, commandEffectUserOutput},
			policies: []string{slashPolicyMutatesConversation, slashPolicyMutatesSessionStore},
		},
		{
			name:     "suggestions",
			effects:  []string{commandEffectLLMProviderRead, commandEffectSessionWrite, commandEffectStateWrite, commandEffectUserOutput},
			policies: []string{slashPolicyMutatesSessionStore, slashPolicyCallsLLM},
		},
		{
			name:     "save-code",
			effects:  []string{commandEffectFilesystemWrite, commandEffectUserOutput},
			policies: []string{slashPolicyMutatesFilesystem},
		},
		{
			name:     "apply-patch",
			effects:  []string{commandEffectProcessExecute, commandEffectWorktreeWrite, commandEffectUserOutput},
			policies: []string{slashPolicyRunsLocalProcess, slashPolicyMutatesWorktree},
		},
		{
			name:     "eval",
			effects:  []string{commandEffectFilesystemRead, commandEffectFilesystemWrite, commandEffectSessionRead, commandEffectUserOutput},
			policies: []string{slashPolicyMutatesFilesystem},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			command, ok := lookupSlashCommand(tt.name)
			require.True(t, ok)
			assert.ElementsMatch(t, tt.effects, command.SideEffects)
			assert.ElementsMatch(t, tt.policies, command.PolicyRequirements)
		})
	}
}

func TestSlashCommandVariants_DeclareActionSpecificPolicy(t *testing.T) {
	t.Parallel()

	command, ok := lookupSlashCommand("eval")
	require.True(t, ok)

	variants := make(map[string]slashCommandVariant, len(command.Variants))
	for _, variant := range command.Variants {
		variants[variant.Name] = variant
	}

	require.Contains(t, variants, "add")
	assert.Equal(t, "/eval add", variants["add"].Usage)
	assert.ElementsMatch(t, []string{
		commandEffectFilesystemWrite,
		commandEffectSessionRead,
		commandEffectUserOutput,
	}, variants["add"].SideEffects)
	assert.Equal(t, []string{slashPolicyMutatesFilesystem}, variants["add"].PolicyRequirements)

	require.Contains(t, variants, "run")
	assert.Equal(t, "/eval run", variants["run"].Usage)
	assert.ElementsMatch(t, []string{
		commandEffectFilesystemRead,
		commandEffectUserOutput,
	}, variants["run"].SideEffects)
	assert.Empty(t, variants["run"].PolicyRequirements)
}

func slashCommandDescriptorWithTestEdit(
	command slashCommandDescriptor,
	edit func(*slashCommandDescriptor),
) slashCommandDescriptor {
	edit(&command)

	return command
}

func TestClearSlashCommand_ClearsConversationState(t *testing.T) {
	t.Parallel()

	messages := []llm.Message{
		{Role: llm.RoleUser, Content: "hello"},
		{Role: llm.RoleAssistant, Content: "world"},
	}
	m := model{
		history:      append([]llm.Message(nil), messages...),
		sessionState: session.New("gpt-test", messages),
		tokenUsage:   tokenUsage{InputTokens: 10, CachedInputTokens: 2, OutputTokens: 3, Responses: 1},
	}

	next, cmd, handled := m.handleSlashCommand("/clear")
	require.True(t, handled)
	require.NotNil(t, cmd)
	assert.Empty(t, next.history)
	assert.Empty(t, next.sessionState.Messages)
	assert.Equal(t, tokenUsage{}, next.tokenUsage)
}

func TestForkSlashCommandPreservesAgentLoopBudget(t *testing.T) {
	t.Parallel()

	messages := []llm.Message{
		{Role: llm.RoleUser, Content: "hello"},
		{Role: llm.RoleAssistant, Content: "world"},
	}
	budget := llm.AgentLoopBudget{MaxInputTokens: 100, MaxOutputTokens: 50, MaxCostMicros: 25_000}
	m := model{
		selectedModel:   "gpt-test",
		history:         append([]llm.Message(nil), messages...),
		sessionState:    session.New("gpt-test", messages),
		agentLoopBudget: budget,
	}

	next, cmd, handled := runForkSlashCommand(m, slashForkInput{Count: 1, HasCount: true})
	require.True(t, handled)
	require.NotNil(t, cmd)
	assert.Len(t, next.history, 1)
	assert.Equal(t, budget, next.sessionState.AgentLoopBudget)
}

func TestSuggestionsSlashCommandSessionOptInUpdatesSession(t *testing.T) {
	t.Parallel()

	store := session.NewStore(filepath.Join(t.TempDir(), "sessions"))

	m := model{
		ctx:           context.Background(),
		sessionStore:  store,
		sessionState:  session.Session{ID: "session-id"},
		selectedModel: "suggest/model",
	}

	next, cmd, handled := runSuggestionsSlashCommand(m, slashSuggestionsInput{Mode: string(promptSuggestionConsentSession)})
	require.True(t, handled)
	require.NotNil(t, cmd)
	assert.Equal(t, promptSuggestionConsentSession, next.promptSuggestionConsent)
	assert.Equal(t, string(appconfig.PromptSuggestionPreferenceModelBacked), next.sessionState.PromptSuggestions)

	raw := cmd()
	batch, ok := raw.(tea.BatchMsg)
	require.True(t, ok)
	require.Len(t, batch, 2)

	msg, ok := batch[0]().(sessionSavedMsg)
	require.True(t, ok)
	require.NoError(t, msg.err)

	loaded, err := store.Load("session-id")
	require.NoError(t, err)
	assert.Equal(t, string(appconfig.PromptSuggestionPreferenceModelBacked), loaded.PromptSuggestions)
}

func TestSuggestionsSlashCommandStatusShowsPrivacyAndBudget(t *testing.T) {
	t.Parallel()

	m := model{
		selectedModel:           "suggest/model",
		promptSuggestionConsent: promptSuggestionConsentGlobal,
		idleSuggestionBudget:    defaultIdleSuggestionBudget(),
		idleSuggestionRequests:  3,
		idleSuggestionUsage:     tokenUsage{InputTokens: 40, OutputTokens: 10, Responses: 2},
		idleSuggestionCostUSD:   0.0012,
		sessionState: session.Session{
			BackgroundSuggestions: &session.BackgroundSuggestionUsage{ProviderCalls: 2},
		},
	}

	next, cmd, handled := runSuggestionsSlashCommand(m, slashSuggestionsInput{Show: true})
	require.True(t, handled)
	require.NotNil(t, cmd)
	assert.Equal(t, m.promptSuggestionConsent, next.promptSuggestionConsent)

	output := stripANSI(toStringMsg(cmd()))
	assert.Contains(t, output, "suggestions: mode=global")
	assert.Contains(t, output, "model=suggest/model")
	assert.Contains(t, output, "budget=requests≤20 rate≤6/min")
	assert.Contains(t, output, "usage=requests=3/20 rate=0/6_per_min provider_calls=2 responses=2 session_tokens=50/12000 cost=$0.001200/$0.05")
	assert.Contains(t, output, "privacy=file/task/issue context omitted")
}

func TestSuggestionsSlashCommandFolderOptInPersistsState(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	store := appconfig.NewStateStore(filepath.Join(dir, "state.yaml"))
	cwd := filepath.Join(dir, "repo")
	require.NoError(t, os.MkdirAll(cwd, 0o750))

	m := model{
		ctx:          context.Background(),
		cwd:          cwd,
		stateStore:   store,
		sessionState: session.Session{ID: "session-id"},
	}

	next, cmd, handled := runSuggestionsSlashCommand(m, slashSuggestionsInput{Mode: string(promptSuggestionConsentFolder)})
	require.True(t, handled)
	require.NotNil(t, cmd)
	assert.Equal(t, promptSuggestionConsentFolder, next.promptSuggestionConsent)
	assert.Empty(t, next.sessionState.PromptSuggestions)

	raw := cmd()
	batch, ok := raw.(tea.BatchMsg)
	require.True(t, ok)
	require.Len(t, batch, 3)

	msg, ok := batch[1]().(promptSuggestionPreferenceSavedMsg)
	require.True(t, ok)
	require.NoError(t, msg.err)

	loaded, err := store.Load()
	require.NoError(t, err)
	assert.Equal(t, appconfig.PromptSuggestionPreferenceModelBacked, loaded.PromptSuggestionsForFolder(cwd))
}

func TestSuggestionsSlashCommandGlobalOptInPersistsState(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	store := appconfig.NewStateStore(filepath.Join(dir, "state.yaml"))
	cwd := filepath.Join(dir, "repo")
	require.NoError(t, os.MkdirAll(cwd, 0o750))

	m := model{
		ctx:          context.Background(),
		cwd:          cwd,
		stateStore:   store,
		sessionState: session.Session{ID: "session-id", PromptSuggestions: string(appconfig.PromptSuggestionPreferenceLocalOnly)},
	}

	next, cmd, handled := runSuggestionsSlashCommand(m, slashSuggestionsInput{Mode: string(promptSuggestionConsentGlobal)})
	require.True(t, handled)
	require.NotNil(t, cmd)
	assert.Equal(t, promptSuggestionConsentGlobal, next.promptSuggestionConsent)
	assert.Empty(t, next.sessionState.PromptSuggestions)

	raw := cmd()
	batch, ok := raw.(tea.BatchMsg)
	require.True(t, ok)
	require.Len(t, batch, 3)

	msg, ok := batch[1]().(promptSuggestionPreferenceSavedMsg)
	require.True(t, ok)
	require.NoError(t, msg.err)

	loaded, err := store.Load()
	require.NoError(t, err)
	assert.Equal(t, appconfig.PromptSuggestionPreferenceModelBacked, loaded.PromptSuggestionsForFolder(filepath.Join(dir, "other-repo")))
}

func TestSuggestionsSlashCommandLocalPersistsStateAndClearsSuggestion(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	store := appconfig.NewStateStore(filepath.Join(dir, "state.yaml"))
	cwd := filepath.Join(dir, "repo")
	require.NoError(t, os.MkdirAll(cwd, 0o750))

	m := model{
		ctx:                     context.Background(),
		cwd:                     cwd,
		stateStore:              store,
		sessionState:            session.Session{ID: "session-id", PromptSuggestions: string(appconfig.PromptSuggestionPreferenceModelBacked)},
		promptSuggestionConsent: promptSuggestionConsentFolder,
		idleSuggestionInput:     "draft",
		idleSuggestionText:      " suffix",
		idleSuggestionStatus:    idleSuggestionStatusReadyModel,
	}

	next, cmd, handled := runSuggestionsSlashCommand(m, slashSuggestionsInput{Mode: string(promptSuggestionConsentLocalOnly)})
	require.True(t, handled)
	require.NotNil(t, cmd)
	assert.Equal(t, promptSuggestionConsentLocalOnly, next.promptSuggestionConsent)
	assert.Equal(t, string(appconfig.PromptSuggestionPreferenceLocalOnly), next.sessionState.PromptSuggestions)
	assert.Empty(t, next.idleSuggestionInput)
	assert.Empty(t, next.idleSuggestionText)
	assert.Empty(t, next.idleSuggestionStatus)

	raw := cmd()
	batch, ok := raw.(tea.BatchMsg)
	require.True(t, ok)
	require.Len(t, batch, 3)

	msg, ok := batch[1]().(promptSuggestionPreferenceSavedMsg)
	require.True(t, ok)
	require.NoError(t, msg.err)

	loaded, err := store.Load()
	require.NoError(t, err)
	assert.Equal(t, appconfig.PromptSuggestionPreferenceLocalOnly, loaded.PromptSuggestionsForFolder(cwd))
}

func TestSuggestionsSlashCommandLocalDisablesGlobalOptInGlobally(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	store := appconfig.NewStateStore(filepath.Join(dir, "state.yaml"))
	cwd := filepath.Join(dir, "repo")
	require.NoError(t, os.MkdirAll(cwd, 0o750))

	require.NoError(t, store.Save(appconfig.State{
		DefaultPromptSuggestions: string(appconfig.PromptSuggestionPreferenceModelBacked),
	}))

	m := model{
		ctx:                     context.Background(),
		cwd:                     cwd,
		stateStore:              store,
		sessionState:            session.Session{ID: "session-id"},
		promptSuggestionConsent: promptSuggestionConsentGlobal,
	}

	next, cmd, handled := runSuggestionsSlashCommand(m, slashSuggestionsInput{Mode: string(promptSuggestionConsentLocalOnly)})
	require.True(t, handled)
	require.NotNil(t, cmd)
	assert.Equal(t, promptSuggestionConsentLocalOnly, next.promptSuggestionConsent)

	raw := cmd()
	batch, ok := raw.(tea.BatchMsg)
	require.True(t, ok)
	require.Len(t, batch, 3)

	msg, ok := batch[1]().(promptSuggestionPreferenceSavedMsg)
	require.True(t, ok)
	require.NoError(t, msg.err)
	assert.Equal(t, appconfig.ModelScopeGlobal, msg.scope)

	loaded, err := store.Load()
	require.NoError(t, err)
	assert.Equal(t, appconfig.PromptSuggestionPreferenceLocalOnly, loaded.PromptSuggestionsForFolder(filepath.Join(dir, "other-repo")))
}

func TestSuggestionsSlashCommandNoNetworkAliasDisablesModelBackedSuggestions(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	store := appconfig.NewStateStore(filepath.Join(dir, "state.yaml"))
	cwd := filepath.Join(dir, "repo")
	require.NoError(t, os.MkdirAll(cwd, 0o750))

	m := model{
		ctx:                     context.Background(),
		cwd:                     cwd,
		stateStore:              store,
		sessionState:            session.Session{ID: "session-id", PromptSuggestions: string(appconfig.PromptSuggestionPreferenceModelBacked)},
		promptSuggestionConsent: promptSuggestionConsentSession,
		idleSuggestionInput:     "draft",
		idleSuggestionText:      " suffix",
		idleSuggestionStatus:    idleSuggestionStatusReadyModel,
	}

	next, cmd, handled := m.handleSlashCommand("/suggestions no-network")
	require.True(t, handled)
	require.NotNil(t, cmd)
	assert.Equal(t, promptSuggestionConsentLocalOnly, next.promptSuggestionConsent)
	assert.Equal(t, string(appconfig.PromptSuggestionPreferenceLocalOnly), next.sessionState.PromptSuggestions)
	assert.Empty(t, next.idleSuggestionInput)
	assert.Empty(t, next.idleSuggestionText)
	assert.Empty(t, next.idleSuggestionStatus)

	raw := cmd()
	batch, ok := raw.(tea.BatchMsg)
	require.True(t, ok)
	require.Len(t, batch, 3)

	msg, ok := batch[1]().(promptSuggestionPreferenceSavedMsg)
	require.True(t, ok)
	require.NoError(t, msg.err)

	loaded, err := store.Load()
	require.NoError(t, err)
	assert.Equal(t, appconfig.PromptSuggestionPreferenceLocalOnly, loaded.PromptSuggestionsForFolder(cwd))
}

func TestSaveCodeSlashCommand_WritesSelectedCodeBlock(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "out.go")
	m := model{
		history: []llm.Message{
			{Role: llm.RoleAssistant, Content: "first\n```go\nfmt.Println(\"one\")\n```\nsecond\n```txt\ntwo\n```"},
		},
	}

	_, cmd, handled := runSaveCodeSlashCommand(m, slashSaveCodeInput{Block: 2, Path: path})
	require.True(t, handled)
	require.NotNil(t, cmd)

	data, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, "two", string(data))
}

func TestExportSlashCommand_BlocksLowAutonomyFilesystemWrite(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "session.md")
	m := model{
		autonomy:     autonomy.Low,
		sessionState: session.New("gpt-test", []llm.Message{{Role: llm.RoleUser, Content: "hello"}}),
	}

	next, cmd, handled := runExportSlashCommand(m, slashOptionalValueInput{Value: path})
	require.True(t, handled)
	require.NotNil(t, cmd)
	assert.Equal(t, autonomy.Low, next.autonomy)
	assert.Contains(t, stripANSI(toStringMsg(cmd())), "autonomy low blocks file writes")

	_, err := os.Stat(path)
	require.ErrorIs(t, err, os.ErrNotExist)
}

func TestSaveCodeSlashCommand_BlocksLowAutonomy(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "out.go")
	m := model{
		autonomy: autonomy.Low,
		history: []llm.Message{
			{Role: llm.RoleAssistant, Content: "```go\nfmt.Println(\"one\")\n```"},
		},
	}

	next, cmd, handled := runSaveCodeSlashCommand(m, slashSaveCodeInput{Block: 1, Path: path})
	require.True(t, handled)
	require.NotNil(t, cmd)
	assert.Equal(t, autonomy.Low, next.autonomy)

	_, err := os.Stat(path)
	require.ErrorIs(t, err, os.ErrNotExist)
}

func TestCopySlashCommands_BlockLowAutonomyClipboardWrites(t *testing.T) {
	t.Parallel()

	m := model{
		autonomy: autonomy.Low,
		history: []llm.Message{
			{Role: llm.RoleAssistant, Content: "answer\n```go\nfmt.Println(\"one\")\n```"},
		},
	}

	next, cmd, handled := runCopySlashCommand(m, slashCopyInput{Target: "last"})
	require.True(t, handled)
	require.NotNil(t, cmd)
	assert.Equal(t, autonomy.Low, next.autonomy)
	assert.Contains(t, stripANSI(toStringMsg(cmd())), "autonomy low blocks mutating shell commands")

	next, cmd, handled = runCopyCodeSlashCommand(m, slashCopyCodeInput{Block: 1})
	require.True(t, handled)
	require.NotNil(t, cmd)
	assert.Equal(t, autonomy.Low, next.autonomy)
	assert.Contains(t, stripANSI(toStringMsg(cmd())), "autonomy low blocks mutating shell commands")
}

func TestMutatingSlashCommands_BlockLowAutonomySessionWrites(t *testing.T) {
	t.Parallel()

	baseMessages := []llm.Message{
		{Role: llm.RoleUser, Content: "hello"},
		{Role: llm.RoleAssistant, Content: "plan"},
	}

	tests := []struct {
		run    func(model) (model, tea.Cmd, bool)
		check  func(*testing.T, model)
		name   string
		detail string
	}{
		{
			name:   "clear",
			detail: "/clear",
			run: func(m model) (model, tea.Cmd, bool) {
				return runClearSlashCommand(m, slashNoArgsInput{})
			},
			check: func(t *testing.T, next model) {
				t.Helper()
				assert.Len(t, next.history, 2)
				assert.Len(t, next.sessionState.Messages, 2)
			},
		},
		{
			name:   "model",
			detail: "/model",
			run: func(m model) (model, tea.Cmd, bool) {
				return runModelSlashCommand(m, slashOptionalValueInput{Value: "new-model"})
			},
			check: func(t *testing.T, next model) {
				t.Helper()
				assert.Equal(t, "old-model", next.selectedModel)
				assert.False(t, next.modelLocked)
				assert.Equal(t, "old-model", next.sessionState.DefaultModel)
			},
		},
		{
			name:   "profile",
			detail: "/profile",
			run: func(m model) (model, tea.Cmd, bool) {
				return runProfileSlashCommand(m, slashOptionalValueInput{Value: "new-agent"})
			},
			check: func(t *testing.T, next model) {
				t.Helper()
				assert.Equal(t, "old-agent", next.selectedAgent)
				assert.Equal(t, "old-agent", next.sessionState.DefaultAgent)
			},
		},
		{
			name:   "save",
			detail: "/save",
			run: func(m model) (model, tea.Cmd, bool) {
				return runSaveSlashCommand(m, slashNoArgsInput{})
			},
		},
		{
			name:   "pin",
			detail: "/pin",
			run: func(m model) (model, tea.Cmd, bool) {
				return runPinSlashCommand(m, slashMessageNumberInput{Number: 2})
			},
			check: func(t *testing.T, next model) {
				t.Helper()
				assert.False(t, next.pinnedMessages[1])
			},
		},
		{
			name:   "context prune",
			detail: "/context prune",
			run: func(m model) (model, tea.Cmd, bool) {
				return runContextSlashCommand(m, slashContextInput{Prune: true})
			},
			check: func(t *testing.T, next model) {
				t.Helper()
				assert.Len(t, next.history, 2)
				assert.Len(t, next.sessionState.Messages, 2)
			},
		},
		{
			name:   "suggestions",
			detail: "/suggestions",
			run: func(m model) (model, tea.Cmd, bool) {
				return runSuggestionsSlashCommand(m, slashSuggestionsInput{Mode: string(promptSuggestionConsentSession)})
			},
			check: func(t *testing.T, next model) {
				t.Helper()
				assert.Empty(t, next.promptSuggestionConsent)
				assert.Empty(t, next.sessionState.PromptSuggestions)
			},
		},
		{
			name:   "retry",
			detail: "/retry",
			run: func(m model) (model, tea.Cmd, bool) {
				return runRetrySlashCommand(m, slashNoArgsInput{})
			},
		},
		{
			name:   "edit",
			detail: "/edit",
			run: func(m model) (model, tea.Cmd, bool) {
				return runEditSlashCommand(m, slashNoArgsInput{})
			},
		},
		{
			name:   "fork",
			detail: "/fork",
			run: func(m model) (model, tea.Cmd, bool) {
				return runForkSlashCommand(m, slashForkInput{})
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			sessionState := session.New("old-model", append([]llm.Message(nil), baseMessages...))
			sessionState.DefaultAgent = "old-agent"
			m := model{
				autonomy:       autonomy.Low,
				history:        append([]llm.Message(nil), baseMessages...),
				modelLocked:    false,
				pinnedMessages: map[int]bool{0: true},
				selectedAgent:  "old-agent",
				selectedModel:  "old-model",
				sessionState:   sessionState,
			}

			next, cmd, handled := tt.run(m)
			require.True(t, handled)
			require.NotNil(t, cmd)

			output := stripANSI(toStringMsg(cmd()))
			assert.Contains(t, output, "autonomy low blocks file writes")
			assert.Contains(t, output, tt.detail)

			if tt.check != nil {
				tt.check(t, next)
			}
		})
	}
}

func TestCountEvalCases_CountsOnlyTopLevelJSONFiles(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "one.json"), []byte("{}"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "two.JSON"), []byte("{}"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "note.txt"), []byte("ignore"), 0o600))
	require.NoError(t, os.Mkdir(filepath.Join(dir, "nested"), 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "nested", "three.json"), []byte("{}"), 0o600))

	got, err := countEvalCases(dir)
	require.NoError(t, err)
	assert.Equal(t, 2, got)
}

func TestEvalSlashCommand_AddWritesCaseAndRunCountsCases(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	sessionState := session.New("gpt-test", []llm.Message{{Role: llm.RoleUser, Content: "hello"}})
	m := model{
		cwd:          dir,
		sessionState: sessionState,
	}

	next, _, handled := runEvalSlashCommand(m, slashEvalInput{Action: "add"})
	require.True(t, handled)
	assert.Equal(t, m.sessionState, next.sessionState)

	path := filepath.Join(dir, ".atteler", "evals", sessionState.ID+".json")
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Contains(t, string(data), `"id": "`+sessionState.ID+`"`)
	assert.Contains(t, string(data), `"role": "user"`)

	count, err := countEvalCases(filepath.Dir(path))
	require.NoError(t, err)
	assert.Equal(t, 1, count)
}

func TestEvalSlashCommand_BlocksLowAutonomyAdd(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	sessionState := session.New("gpt-test", []llm.Message{{Role: llm.RoleUser, Content: "hello"}})
	m := model{
		autonomy:     autonomy.Low,
		cwd:          dir,
		sessionState: sessionState,
	}

	next, cmd, handled := runEvalSlashCommand(m, slashEvalInput{Action: "add"})
	require.True(t, handled)
	require.NotNil(t, cmd)
	assert.Equal(t, autonomy.Low, next.autonomy)
	assert.Contains(t, stripANSI(toStringMsg(cmd())), "autonomy low blocks file writes")

	path := filepath.Join(dir, ".atteler", "evals", sessionState.ID+".json")
	_, err := os.Stat(path)
	require.ErrorIs(t, err, os.ErrNotExist)
}

func TestEvalSlashCommand_RunCountsCasesWithoutShellTask(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	casesDir := filepath.Join(dir, ".atteler", "evals")
	require.NoError(t, os.MkdirAll(casesDir, 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(casesDir, "one.json"), []byte("{}"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(casesDir, "two.json"), []byte("{}"), 0o600))

	m := model{cwd: dir, waiting: false}

	next, cmd, handled := runEvalSlashCommand(m, slashEvalInput{Action: "run"})
	require.True(t, handled)
	require.NotNil(t, cmd)
	assert.False(t, next.waiting, "/eval run should count cases directly instead of starting a shell task")
}

func TestCommandSurface_IncludesSlashCommandDescriptors(t *testing.T) {
	t.Parallel()

	surface := buildCommandSurface(buildCommandRegistry())
	require.NotEmpty(t, surface.SlashCommands)

	commands := make(map[string]commandSurfaceSlashCommand, len(surface.SlashCommands))
	for _, command := range surface.SlashCommands {
		commands[command.Name] = command
	}

	require.Contains(t, commands, "apply-patch")
	assert.Equal(t, "/apply-patch", commands["apply-patch"].Usage)
	assert.Empty(t, commands["apply-patch"].InputFields)
	assert.Equal(t, []string{"patch"}, commands["apply-patch"].CompletionTokens)
	assert.Contains(t, commands["apply-patch"].SideEffects, commandEffectProcessExecute)
	assert.Contains(t, commands["apply-patch"].PolicyRequirements, slashPolicyMutatesWorktree)

	require.Contains(t, commands, "save-code")
	assert.Equal(t, "slashSaveCodeInput", commands["save-code"].InputType)
	assert.Equal(t, []string{"Path", "Block"}, commands["save-code"].InputFields)
	assert.Equal(t, []slashCommandArgument{
		{Name: "n", Type: "int", Required: true},
		{Name: "path", Type: "string", Required: true},
	}, commands["save-code"].Arguments)

	require.Contains(t, commands, "copy-code")
	assert.Equal(t, "/copy-code [n]", commands["copy-code"].Usage)
	assert.Equal(t, []slashCommandArgument{
		{Name: "n", Type: "int"},
	}, commands["copy-code"].Arguments)

	require.Contains(t, commands, "mode")
	assert.Equal(t, []string{"Mode", "Show"}, commands["mode"].InputFields)
	assert.Equal(t, "/mode [plan|execute]", commands["mode"].Usage)
	assert.Equal(t, []string{"plan", "execute"}, commands["mode"].CompletionTokens)
	assert.Equal(t, []slashCommandArgument{
		{Name: "mode", Type: slashArgumentTypeEnum, Values: []string{"plan", "execute"}},
	}, commands["mode"].Arguments)

	require.Contains(t, commands, "suggestions")
	assert.Equal(t, []string{"Mode", "Show"}, commands["suggestions"].InputFields)
	assert.Equal(t, "/suggestions [status|local|session|folder|global]", commands["suggestions"].Usage)
	assert.Contains(t, commands["suggestions"].PolicyRequirements, slashPolicyMutatesSessionStore)
	assert.Contains(t, commands["suggestions"].PolicyRequirements, slashPolicyCallsLLM)
	assert.Contains(t, commands["suggestions"].SideEffects, commandEffectLLMProviderRead)
	assert.Contains(t, commands["suggestions"].SideEffects, commandEffectStateWrite)
	assert.Equal(t, []slashCommandArgument{
		{Name: "mode", Type: slashArgumentTypeEnum, Values: []string{"status", "local", string(promptSuggestionConsentSession), string(promptSuggestionConsentFolder), string(promptSuggestionConsentGlobal)}},
	}, commands["suggestions"].Arguments)

	require.Contains(t, commands, "export")
	assert.Contains(t, commands["export"].SharedCLICommands, "session-read")
	assert.Contains(t, commands["export"].SideEffects, commandEffectSessionRead)

	require.Contains(t, commands, "eval")
	require.Len(t, commands["eval"].Variants, 2)
	assert.Contains(t, commands["eval"].PolicyRequirements, slashPolicyMutatesFilesystem)
}

func knownSlashPolicyRequirementsForTest() map[string]bool {
	return map[string]bool{
		slashPolicyMutatesConversation: true,
		slashPolicyMutatesSessionStore: true,
		slashPolicyMutatesFilesystem:   true,
		slashPolicyMutatesWorktree:     true,
		slashPolicyRunsLocalProcess:    true,
		slashPolicyUsesClipboard:       true,
		slashPolicyCallsLLM:            true,
	}
}

func slashSchemaSampleArgsForTest(arguments []slashCommandArgument) []string {
	args := make([]string, 0, len(arguments))

	for _, argument := range arguments {
		args = append(args, slashArgumentSampleValueForTest(argument))
	}

	return args
}

func slashArgumentSampleValueForTest(argument slashCommandArgument) string {
	switch argument.Type {
	case slashArgumentTypeEnum:
		if len(argument.Values) > 0 {
			return argument.Values[0]
		}
	case "int":
		return "1"
	}

	return "value"
}

func slashSchemaHasVariadicArgumentForTest(arguments []slashCommandArgument) bool {
	for _, argument := range arguments {
		if argument.Variadic {
			return true
		}
	}

	return false
}
