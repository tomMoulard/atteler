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

func TestSlashHelp_Golden(t *testing.T) {
	t.Parallel()

	want := `/help /model [name] /profile [name] /save /export [path] /clear /retry /edit /fork [n]
/tokens /cost /search <query> /pin <n> /unpin <n> /context [prune] /mode [plan|execute]
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
