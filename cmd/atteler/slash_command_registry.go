package main

import (
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"unicode"

	tea "github.com/charmbracelet/bubbletea"
)

const (
	slashPolicyMutatesConversation = "mutates-conversation"
	slashPolicyMutatesSessionStore = "mutates-session-store"
	slashPolicyMutatesFilesystem   = "mutates-filesystem"
	slashPolicyMutatesWorktree     = "mutates-worktree"
	slashPolicyRunsLocalProcess    = "runs-local-process"
	slashPolicyUsesClipboard       = "uses-clipboard"
	slashPolicyCallsLLM            = "calls-llm"

	slashArgumentTypeEnum = "enum"
)

type slashCommandParser func([]string) (any, error)

type slashCommandHandler func(model, any) (model, tea.Cmd, bool)

type typedSlashCommandParser[I any] func([]string, string) (I, error)

type slashCommandDescriptor struct {
	Parse               slashCommandParser
	Run                 slashCommandHandler
	Name                string
	Usage               string
	Summary             string
	InputType           string
	InputFields         []string
	Arguments           []slashCommandArgument
	Aliases             []string
	SharedCLICommands   []string
	Variants            []slashCommandVariant
	SideEffects         []string
	OutputModes         []string
	PolicyRequirements  []string
	CompletionTokens    []string
	HelpAliases         []string
	HelpGroup           int
	MutatesConversation bool
	MutatesSessionStore bool
	MutatesFilesystem   bool
	MutatesWorktree     bool
	RunsLocalProcess    bool
	UsesClipboard       bool
	CallsLLM            bool
}

type slashCommandArgument struct {
	Name     string   `json:"name"`
	Type     string   `json:"type"`
	Values   []string `json:"values,omitempty"`
	Required bool     `json:"required,omitempty"`
	Variadic bool     `json:"variadic,omitempty"`
}

type slashCommandVariant struct {
	Name               string   `json:"name"`
	Usage              string   `json:"usage"`
	Summary            string   `json:"summary"`
	SideEffects        []string `json:"side_effects"`
	OutputModes        []string `json:"output_modes"`
	PolicyRequirements []string `json:"policy_requirements,omitempty"`
}

type slashNoArgsInput struct{}

type slashOptionalValueInput struct {
	Value string
}

type slashForkInput struct {
	Count    int
	HasCount bool
}

type slashSearchInput struct {
	Query string
}

type slashMessageNumberInput struct {
	Number int
}

type slashContextInput struct {
	Prune bool
}

type slashModeInput struct {
	Mode string
	Show bool
}

type slashSuggestionsInput struct {
	Mode string
	Show bool
}

type slashSaveCodeInput struct {
	Path  string
	Block int
}

type slashCopyInput struct {
	Target string
}

type slashCopyCodeInput struct {
	Block int
}

type slashEvalInput struct {
	Action string
}

func typedSlashCommand[I any](
	descriptor slashCommandDescriptor,
	parse typedSlashCommandParser[I],
	run func(model, I) (model, tea.Cmd, bool),
) slashCommandDescriptor {
	descriptor.InputType = slashInputTypeName[I]()
	descriptor.InputFields = slashInputFieldNames[I]()
	descriptor.Parse = func(args []string) (any, error) {
		return parse(args, descriptor.Usage)
	}
	descriptor.Run = func(m model, input any) (model, tea.Cmd, bool) {
		typedInput, ok := input.(I)
		if !ok {
			return m, tea.Println(errStyle.Render("internal slash command input type mismatch")), true
		}

		return run(m, typedInput)
	}

	descriptor.SideEffects = mergedSlashSideEffects(descriptor)
	descriptor.OutputModes = mergedSlashOutputModes(descriptor)
	descriptor.PolicyRequirements = mergedSlashPolicyRequirements(descriptor)

	return descriptor
}

func slashInputTypeName[I any]() string {
	valueType := slashInputReflectType[I]()
	if valueType == nil {
		return ""
	}

	return valueType.Name()
}

func slashInputFieldNames[I any]() []string {
	valueType := slashInputReflectType[I]()
	if valueType == nil || valueType.Kind() != reflect.Struct {
		return nil
	}

	fields := make([]string, 0, valueType.NumField())
	for field := range valueType.Fields() {
		if field.PkgPath == "" {
			fields = append(fields, field.Name)
		}
	}

	return fields
}

func slashInputReflectType[I any]() reflect.Type {
	var zero I

	valueType := reflect.TypeOf(zero)
	if valueType == nil {
		return nil
	}

	if valueType.Kind() == reflect.Pointer {
		valueType = valueType.Elem()
	}

	return valueType
}

func slashCommandDescriptors() []slashCommandDescriptor {
	commands := []slashCommandDescriptor{
		typedSlashCommand(
			slashCommandDescriptor{
				Name:             helpCommandName,
				Usage:            "/help",
				Summary:          "show interactive slash command help",
				SideEffects:      []string{commandEffectUserOutput},
				OutputModes:      []string{commandOutputText},
				CompletionTokens: []string{"commands"},
				HelpGroup:        0,
			},
			parseNoArgs,
			runHelpSlashCommand,
		),
		typedSlashCommand(
			slashCommandDescriptor{
				Name:                "model",
				Usage:               "/model [name]",
				Summary:             "show or change the active model",
				Arguments:           []slashCommandArgument{optionalSlashArg("name", "string")},
				SideEffects:         []string{commandEffectSessionWrite, commandEffectUserOutput},
				OutputModes:         []string{commandOutputText},
				CompletionTokens:    []string{"provider"},
				HelpGroup:           0,
				MutatesSessionStore: true,
			},
			parseOptionalValue,
			runModelSlashCommand,
		),
		typedSlashCommand(
			slashCommandDescriptor{
				Name:                "profile",
				Usage:               "/profile [name]",
				Summary:             "show or change the configured agent profile",
				Arguments:           []slashCommandArgument{optionalSlashArg("name", "string")},
				SideEffects:         []string{commandEffectConfigRead, commandEffectSessionWrite, commandEffectUserOutput},
				OutputModes:         []string{commandOutputText},
				CompletionTokens:    []string{"agent"},
				HelpGroup:           0,
				MutatesSessionStore: true,
			},
			parseOptionalValue,
			runProfileSlashCommand,
		),
		typedSlashCommand(
			slashCommandDescriptor{
				Name:                "save",
				Usage:               "/save",
				Summary:             "save the current session",
				SideEffects:         []string{commandEffectSessionWrite, commandEffectUserOutput},
				OutputModes:         []string{commandOutputText},
				PolicyRequirements:  []string{slashPolicyMutatesSessionStore},
				HelpGroup:           0,
				MutatesSessionStore: true,
			},
			parseNoArgs,
			runSaveSlashCommand,
		),
		typedSlashCommand(
			slashCommandDescriptor{
				Name:               "export",
				Usage:              "/export [path]",
				Summary:            "export the current session transcript",
				Arguments:          []slashCommandArgument{optionalSlashArg("path", "string")},
				SharedCLICommands:  []string{"session-read"},
				SideEffects:        []string{commandEffectFilesystemWrite, commandEffectUserOutput},
				OutputModes:        []string{commandOutputFilesystem, commandOutputJSON, commandOutputMarkdown, commandOutputText},
				PolicyRequirements: []string{slashPolicyMutatesFilesystem},
				CompletionTokens:   []string{sessionCommandName},
				HelpGroup:          0,
				MutatesFilesystem:  true,
			},
			parseOptionalValue,
			runExportSlashCommand,
		),
		typedSlashCommand(
			slashCommandDescriptor{
				Name:                "clear",
				Usage:               "/clear",
				Summary:             "clear the visible conversation and persist the empty session",
				SideEffects:         []string{commandEffectSessionWrite, commandEffectUserOutput},
				OutputModes:         []string{commandOutputText},
				PolicyRequirements:  []string{slashPolicyMutatesConversation, slashPolicyMutatesSessionStore},
				HelpGroup:           0,
				MutatesConversation: true,
				MutatesSessionStore: true,
			},
			parseNoArgs,
			runClearSlashCommand,
		),
		typedSlashCommand(
			slashCommandDescriptor{
				Name:                "retry",
				Usage:               "/retry",
				Summary:             "regenerate the last user prompt",
				Aliases:             []string{"regenerate"},
				SideEffects:         []string{commandEffectLLMProviderRead, commandEffectSessionWrite, commandEffectUserOutput},
				OutputModes:         []string{commandOutputText},
				CompletionTokens:    []string{"regenerate"},
				HelpGroup:           0,
				MutatesConversation: true,
				MutatesSessionStore: true,
				CallsLLM:            true,
			},
			parseNoArgs,
			runRetrySlashCommand,
		),
		typedSlashCommand(
			slashCommandDescriptor{
				Name:                "edit",
				Usage:               "/edit",
				Summary:             "edit the last user prompt",
				SideEffects:         []string{commandEffectSessionWrite, commandEffectUserOutput},
				OutputModes:         []string{commandOutputText},
				PolicyRequirements:  []string{slashPolicyMutatesConversation},
				HelpGroup:           0,
				MutatesConversation: true,
			},
			parseNoArgs,
			runEditSlashCommand,
		),
		typedSlashCommand(
			slashCommandDescriptor{
				Name:                "fork",
				Usage:               "/fork [n]",
				Summary:             "fork the current session at a message index",
				Arguments:           []slashCommandArgument{optionalSlashArg("n", "int")},
				SideEffects:         []string{commandEffectSessionWrite, commandEffectUserOutput},
				OutputModes:         []string{commandOutputText},
				PolicyRequirements:  []string{slashPolicyMutatesConversation},
				CompletionTokens:    []string{sessionCommandName},
				HelpGroup:           0,
				MutatesConversation: true,
			},
			parseForkInput,
			runForkSlashCommand,
		),
		typedSlashCommand(
			slashCommandDescriptor{
				Name:             "tokens",
				Usage:            "/tokens",
				Summary:          "show token usage and estimated cost",
				Aliases:          []string{"cost"},
				HelpAliases:      []string{"cost"},
				SideEffects:      []string{commandEffectUserOutput},
				OutputModes:      []string{commandOutputText},
				CompletionTokens: []string{"cost"},
				HelpGroup:        1,
			},
			parseNoArgs,
			runTokensSlashCommand,
		),
		typedSlashCommand(
			slashCommandDescriptor{
				Name:             "search",
				Usage:            "/search <query>",
				Summary:          "search the current conversation",
				Arguments:        []slashCommandArgument{{Name: "query", Type: "string", Required: true, Variadic: true}},
				SideEffects:      []string{commandEffectUserOutput},
				OutputModes:      []string{commandOutputText},
				CompletionTokens: []string{sessionCommandName},
				HelpGroup:        1,
			},
			parseSearchInput,
			runSearchSlashCommand,
		),
		typedSlashCommand(
			slashCommandDescriptor{
				Name:                "pin",
				Usage:               "/pin <n>",
				Summary:             "pin a message before pruning context",
				Arguments:           []slashCommandArgument{requiredSlashArg("n", "int")},
				SideEffects:         []string{commandEffectUserOutput},
				OutputModes:         []string{commandOutputText},
				PolicyRequirements:  []string{slashPolicyMutatesConversation},
				CompletionTokens:    []string{"context"},
				HelpGroup:           1,
				MutatesConversation: true,
			},
			parseMessageNumberInput,
			runPinSlashCommand,
		),
		typedSlashCommand(
			slashCommandDescriptor{
				Name:                "unpin",
				Usage:               "/unpin <n>",
				Summary:             "unpin a message",
				Arguments:           []slashCommandArgument{requiredSlashArg("n", "int")},
				SideEffects:         []string{commandEffectUserOutput},
				OutputModes:         []string{commandOutputText},
				PolicyRequirements:  []string{slashPolicyMutatesConversation},
				CompletionTokens:    []string{"context"},
				HelpGroup:           1,
				MutatesConversation: true,
			},
			parseMessageNumberInput,
			runUnpinSlashCommand,
		),
		typedSlashCommand(
			slashCommandDescriptor{
				Name:                "context",
				Usage:               "/context [prune]",
				Summary:             "show or prune conversation context",
				Arguments:           []slashCommandArgument{{Name: "prune", Type: slashArgumentTypeEnum, Values: []string{"prune"}}},
				SideEffects:         []string{commandEffectUserOutput},
				OutputModes:         []string{commandOutputText},
				PolicyRequirements:  []string{slashPolicyMutatesConversation},
				HelpGroup:           1,
				MutatesConversation: true,
			},
			parseContextInput,
			runContextSlashCommand,
		),
		typedSlashCommand(
			slashCommandDescriptor{
				Name:                "mode",
				Usage:               "/mode [plan|execute]",
				Summary:             "show or switch between plan and execute modes",
				Arguments:           []slashCommandArgument{{Name: "mode", Type: slashArgumentTypeEnum, Values: []string{"plan", "execute"}}},
				SideEffects:         []string{commandEffectUserOutput},
				OutputModes:         []string{commandOutputText},
				PolicyRequirements:  []string{slashPolicyMutatesConversation},
				CompletionTokens:    []string{"plan", "execute"},
				HelpGroup:           1,
				MutatesConversation: true,
			},
			parseModeInput,
			runModeSlashCommand,
		),
		typedSlashCommand(
			slashCommandDescriptor{
				Name:                "suggestions",
				Usage:               "/suggestions [status|local|session|folder|global]",
				Summary:             "show or opt in to model-backed idle prompt suggestions",
				Arguments:           []slashCommandArgument{{Name: "mode", Type: slashArgumentTypeEnum, Values: []string{"status", "local", string(promptSuggestionConsentSession), string(promptSuggestionConsentFolder), string(promptSuggestionConsentGlobal)}}},
				SideEffects:         []string{commandEffectLLMProviderRead, commandEffectSessionWrite, commandEffectStateWrite, commandEffectUserOutput},
				OutputModes:         []string{commandOutputText},
				CompletionTokens:    []string{"prompt", "privacy", "local", "provider"},
				HelpGroup:           1,
				MutatesSessionStore: true,
				CallsLLM:            true,
			},
			parseSuggestionsInput,
			runSuggestionsSlashCommand,
		),
		typedSlashCommand(
			slashCommandDescriptor{
				Name:             "template",
				Usage:            "/template [name]",
				Summary:          "insert a local prompt template",
				Arguments:        []slashCommandArgument{optionalSlashArg("name", "string")},
				SideEffects:      []string{commandEffectUserOutput},
				OutputModes:      []string{commandOutputText},
				CompletionTokens: []string{"prompt"},
				HelpGroup:        2,
			},
			parseOptionalValue,
			runTemplateSlashCommand,
		),
		typedSlashCommand(
			slashCommandDescriptor{
				Name:             "codeblocks",
				Usage:            "/codeblocks",
				Summary:          "list code blocks from the last assistant response",
				SideEffects:      []string{commandEffectUserOutput},
				OutputModes:      []string{commandOutputText},
				CompletionTokens: []string{"code"},
				HelpGroup:        2,
			},
			parseNoArgs,
			runCodeblocksSlashCommand,
		),
		typedSlashCommand(
			slashCommandDescriptor{
				Name:               "save-code",
				Usage:              "/save-code <n> <path>",
				Summary:            "save a code block from the last assistant response",
				Arguments:          []slashCommandArgument{requiredSlashArg("n", "int"), requiredSlashArg("path", "string")},
				SideEffects:        []string{commandEffectFilesystemWrite, commandEffectUserOutput},
				OutputModes:        []string{commandOutputFilesystem, commandOutputText},
				PolicyRequirements: []string{slashPolicyMutatesFilesystem},
				CompletionTokens:   []string{"code"},
				HelpGroup:          2,
				MutatesFilesystem:  true,
			},
			parseSaveCodeInput,
			runSaveCodeSlashCommand,
		),
		typedSlashCommand(
			slashCommandDescriptor{
				Name:               "copy",
				Usage:              "/copy [last|session]",
				Summary:            "copy the last answer or full session",
				Arguments:          []slashCommandArgument{{Name: "target", Type: slashArgumentTypeEnum, Values: []string{"last", sessionCommandName}}},
				SideEffects:        []string{commandEffectProcessExecute, commandEffectUserOutput},
				OutputModes:        []string{commandOutputProcess, commandOutputText},
				PolicyRequirements: []string{slashPolicyUsesClipboard},
				HelpGroup:          2,
				UsesClipboard:      true,
			},
			parseCopyInput,
			runCopySlashCommand,
		),
		typedSlashCommand(
			slashCommandDescriptor{
				Name:               "copy-code",
				Usage:              "/copy-code [n]",
				Summary:            "copy a code block, defaulting to the first block",
				Arguments:          []slashCommandArgument{optionalSlashArg("n", "int")},
				SideEffects:        []string{commandEffectProcessExecute, commandEffectUserOutput},
				OutputModes:        []string{commandOutputProcess, commandOutputText},
				PolicyRequirements: []string{slashPolicyUsesClipboard},
				CompletionTokens:   []string{"code"},
				HelpGroup:          2,
				UsesClipboard:      true,
			},
			parseCopyCodeInput,
			runCopyCodeSlashCommand,
		),
		typedSlashCommand(
			slashCommandDescriptor{
				Name:               "apply-patch",
				Usage:              "/apply-patch",
				Summary:            "apply the last assistant unified diff with git apply",
				SideEffects:        []string{commandEffectProcessExecute, commandEffectWorktreeWrite, commandEffectUserOutput},
				OutputModes:        []string{commandOutputProcess, commandOutputText},
				PolicyRequirements: []string{slashPolicyRunsLocalProcess, slashPolicyMutatesWorktree},
				CompletionTokens:   []string{"patch"},
				HelpGroup:          2,
				MutatesWorktree:    true,
				RunsLocalProcess:   true,
			},
			parseNoArgs,
			runApplyPatchSlashCommand,
		),
		typedSlashCommand(
			slashCommandDescriptor{
				Name:      "eval",
				Usage:     "/eval add|run",
				Summary:   "add or count local evaluation cases",
				Arguments: []slashCommandArgument{{Name: "action", Type: slashArgumentTypeEnum, Required: true, Values: []string{"add", "run"}}},
				Variants: []slashCommandVariant{
					{
						Name:               "add",
						Usage:              "/eval add",
						Summary:            "add the current session to local evaluation cases",
						SideEffects:        []string{commandEffectFilesystemWrite, commandEffectSessionRead, commandEffectUserOutput},
						OutputModes:        []string{commandOutputFilesystem, commandOutputText},
						PolicyRequirements: []string{slashPolicyMutatesFilesystem},
					},
					{
						Name:        "run",
						Usage:       "/eval run",
						Summary:     "count local evaluation cases",
						SideEffects: []string{commandEffectFilesystemRead, commandEffectUserOutput},
						OutputModes: []string{commandOutputText},
					},
				},
				CompletionTokens: []string{"test"},
				HelpGroup:        2,
			},
			parseEvalInput,
			runEvalSlashCommand,
		),
	}

	mustValidateSlashCommandDescriptors(commands)

	return commands
}

func mustValidateSlashCommandDescriptors(commands []slashCommandDescriptor) {
	if err := validateSlashCommandDescriptors(commands); err != nil {
		panic(err)
	}
}

func validateSlashCommandDescriptors(commands []slashCommandDescriptor) error {
	if len(commands) == 0 {
		return errors.New("missing slash command descriptors")
	}

	seen := make(map[string]string, len(commands))
	knownCLICommands := knownCLICommandContracts()
	previousHelpGroup := commands[0].HelpGroup

	for i := range commands {
		command := &commands[i]
		if command.HelpGroup < previousHelpGroup {
			return fmt.Errorf("slash command %q help group %d appears after help group %d", command.Name, command.HelpGroup, previousHelpGroup)
		}

		if err := validateSlashCommandDescriptor(command, seen, knownCLICommands); err != nil {
			return err
		}

		previousHelpGroup = command.HelpGroup
	}

	return nil
}

func validateSlashCommandDescriptor(
	command *slashCommandDescriptor,
	seen map[string]string,
	knownCLICommands map[string]bool,
) error {
	if err := validateSlashCommandNamesAndAliases(command, seen); err != nil {
		return err
	}

	if err := validateSlashCommandRequiredMetadata(command); err != nil {
		return err
	}

	if err := validateSlashCommandArguments(command); err != nil {
		return err
	}

	if err := validateSlashCommandUsageArguments(command); err != nil {
		return err
	}

	if err := validateSlashCommandVariants(command); err != nil {
		return err
	}

	if err := validateSlashCommandEffectsAndPolicy(command); err != nil {
		return err
	}

	if err := validateSlashCommandSharedCLI(command, knownCLICommands); err != nil {
		return err
	}

	return nil
}

func validateSlashCommandNamesAndAliases(command *slashCommandDescriptor, seen map[string]string) error {
	if strings.TrimSpace(command.Name) == "" {
		return errors.New("slash command descriptor missing name")
	}

	if strings.HasPrefix(command.Name, "/") {
		return fmt.Errorf("slash command %q name must not include leading slash", command.Name)
	}

	if command.Name != strings.ToLower(command.Name) {
		return fmt.Errorf("slash command %q name must be lowercase", command.Name)
	}

	if err := recordSlashCommandName(command.Name, command.Name, seen); err != nil {
		return err
	}

	for _, alias := range command.Aliases {
		if err := recordSlashCommandName(alias, command.Name, seen); err != nil {
			return err
		}
	}

	for _, alias := range command.HelpAliases {
		if !slices.Contains(command.Aliases, alias) {
			return fmt.Errorf("slash command %q help alias %q is not a parseable alias", command.Name, alias)
		}
	}

	return nil
}

func validateSlashCommandSharedCLI(command *slashCommandDescriptor, knownCLICommands map[string]bool) error {
	for _, shared := range command.SharedCLICommands {
		if !knownCLICommands[shared] {
			return fmt.Errorf("slash command %q references unknown shared CLI command %q", command.Name, shared)
		}
	}

	return nil
}

func recordSlashCommandName(name, owner string, seen map[string]string) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return fmt.Errorf("slash command %q has empty alias", owner)
	}

	if strings.HasPrefix(name, "/") {
		return fmt.Errorf("slash command %q alias %q must not include leading slash", owner, name)
	}

	if name != strings.ToLower(name) {
		return fmt.Errorf("slash command %q alias %q must be lowercase", owner, name)
	}

	if previous, ok := seen[name]; ok {
		return fmt.Errorf("slash command %q shadows %q", name, previous)
	}

	seen[name] = owner

	return nil
}

func validateSlashCommandRequiredMetadata(command *slashCommandDescriptor) error {
	switch {
	case strings.TrimSpace(command.Usage) == "":
		return fmt.Errorf("slash command %q missing usage", command.Name)
	case !strings.HasPrefix(command.Usage, "/"):
		return fmt.Errorf("slash command %q usage must start with slash", command.Name)
	case slashUsageCommandName(command.Usage) != command.Name:
		return fmt.Errorf("slash command %q usage must start with /%s", command.Name, command.Name)
	case strings.TrimSpace(command.Summary) == "":
		return fmt.Errorf("slash command %q missing summary", command.Name)
	case strings.TrimSpace(command.InputType) == "":
		return fmt.Errorf("slash command %q missing input type", command.Name)
	case command.Parse == nil:
		return fmt.Errorf("slash command %q missing parser", command.Name)
	case command.Run == nil:
		return fmt.Errorf("slash command %q missing handler", command.Name)
	case len(command.SideEffects) == 0:
		return fmt.Errorf("slash command %q missing side effects", command.Name)
	case len(command.OutputModes) == 0:
		return fmt.Errorf("slash command %q missing output modes", command.Name)
	}

	return nil
}

func validateSlashCommandArguments(command *slashCommandDescriptor) error {
	seenNames := make(map[string]bool, len(command.Arguments))
	seenOptional := false

	for i := range command.Arguments {
		argument := &command.Arguments[i]
		if strings.TrimSpace(argument.Name) == "" {
			return fmt.Errorf("slash command %q has argument missing name", command.Name)
		}

		if strings.TrimSpace(argument.Type) == "" {
			return fmt.Errorf("slash command %q argument %q missing type", command.Name, argument.Name)
		}

		if seenNames[argument.Name] {
			return fmt.Errorf("slash command %q has duplicate argument %q", command.Name, argument.Name)
		}

		seenNames[argument.Name] = true

		if argument.Type == slashArgumentTypeEnum {
			if err := validateSlashEnumValues(command.Name, argument); err != nil {
				return err
			}
		}

		if argument.Required && seenOptional {
			return fmt.Errorf("slash command %q required argument %q follows an optional argument", command.Name, argument.Name)
		}

		if !argument.Required {
			seenOptional = true
		}

		if argument.Variadic && i != len(command.Arguments)-1 {
			return fmt.Errorf("slash command %q variadic argument %q must be last", command.Name, argument.Name)
		}
	}

	return nil
}

func validateSlashEnumValues(commandName string, argument *slashCommandArgument) error {
	if len(argument.Values) == 0 {
		return fmt.Errorf("slash command %q enum argument %q missing values", commandName, argument.Name)
	}

	seen := make(map[string]bool, len(argument.Values))
	for _, value := range argument.Values {
		switch {
		case strings.TrimSpace(value) == "":
			return fmt.Errorf("slash command %q enum argument %q has empty value", commandName, argument.Name)
		case value != strings.TrimSpace(value):
			return fmt.Errorf("slash command %q enum argument %q value %q must be trimmed", commandName, argument.Name, value)
		case value != strings.ToLower(value):
			return fmt.Errorf("slash command %q enum argument %q value %q must be lowercase", commandName, argument.Name, value)
		case seen[value]:
			return fmt.Errorf("slash command %q enum argument %q has duplicate value %q", commandName, argument.Name, value)
		}

		seen[value] = true
	}

	return nil
}

func validateSlashCommandUsageArguments(command *slashCommandDescriptor) error {
	usageFields := strings.Fields(command.Usage)
	if len(usageFields) == 0 {
		return fmt.Errorf("slash command %q missing usage", command.Name)
	}

	usageArgs := usageFields[1:]
	if len(usageArgs) != len(command.Arguments) {
		return fmt.Errorf("slash command %q usage argument count %d does not match schema argument count %d", command.Name, len(usageArgs), len(command.Arguments))
	}

	for i := range command.Arguments {
		argument := &command.Arguments[i]
		usageArg, required, ok := parseSlashUsageArgument(usageArgs[i])

		if !ok {
			return fmt.Errorf("slash command %q has malformed usage argument %q", command.Name, usageArgs[i])
		}

		if required != argument.Required {
			return fmt.Errorf("slash command %q usage argument %q required marker does not match schema", command.Name, usageArgs[i])
		}

		expected := slashArgumentUsageLabel(argument)
		if usageArg != expected {
			return fmt.Errorf("slash command %q usage argument %q does not match schema argument %q", command.Name, usageArgs[i], expected)
		}
	}

	return nil
}

func parseSlashUsageArgument(value string) (label string, required, ok bool) {
	switch {
	case strings.HasPrefix(value, "<") && strings.HasSuffix(value, ">"):
		return strings.TrimSuffix(strings.TrimPrefix(value, "<"), ">"), true, true
	case strings.HasPrefix(value, "[") && strings.HasSuffix(value, "]"):
		return strings.TrimSuffix(strings.TrimPrefix(value, "["), "]"), false, true
	case value != "":
		return value, true, true
	default:
		return "", false, false
	}
}

func slashArgumentUsageLabel(argument *slashCommandArgument) string {
	if argument.Type == slashArgumentTypeEnum {
		return strings.Join(argument.Values, "|")
	}

	return argument.Name
}

func validateSlashCommandVariants(command *slashCommandDescriptor) error {
	seen := make(map[string]bool, len(command.Variants))

	for i := range command.Variants {
		variant := &command.Variants[i]
		if strings.TrimSpace(variant.Name) == "" {
			return fmt.Errorf("slash command %q has variant missing name", command.Name)
		}

		if variant.Name != strings.ToLower(variant.Name) {
			return fmt.Errorf("slash command %q variant %q must be lowercase", command.Name, variant.Name)
		}

		if strings.HasPrefix(variant.Name, "/") {
			return fmt.Errorf("slash command %q variant %q must not include leading slash", command.Name, variant.Name)
		}

		if seen[variant.Name] {
			return fmt.Errorf("slash command %q has duplicate variant %q", command.Name, variant.Name)
		}

		seen[variant.Name] = true

		switch {
		case strings.TrimSpace(variant.Usage) == "":
			return fmt.Errorf("slash command %q variant %q missing usage", command.Name, variant.Name)
		case !slashUsageStartsWithVariant(variant.Usage, command.Name, variant.Name):
			return fmt.Errorf("slash command %q variant %q usage must start with /%s %s", command.Name, variant.Name, command.Name, variant.Name)
		case strings.TrimSpace(variant.Summary) == "":
			return fmt.Errorf("slash command %q variant %q missing summary", command.Name, variant.Name)
		case len(variant.SideEffects) == 0:
			return fmt.Errorf("slash command %q variant %q missing side effects", command.Name, variant.Name)
		case len(variant.OutputModes) == 0:
			return fmt.Errorf("slash command %q variant %q missing output modes", command.Name, variant.Name)
		}

		if err := validateSlashMetadataValues(command.Name+" "+variant.Name, variant.SideEffects, variant.OutputModes, variant.PolicyRequirements); err != nil {
			return err
		}

		if slashVariantRequiresPolicy(*variant) && len(variant.PolicyRequirements) == 0 {
			return fmt.Errorf("slash command %q variant %q mutates state or crosses policy boundary without policy requirements", command.Name, variant.Name)
		}
	}

	if err := validateSlashCommandVariantArgument(command, seen); err != nil {
		return err
	}

	return nil
}

func validateSlashCommandVariantArgument(command *slashCommandDescriptor, variants map[string]bool) error {
	if len(variants) == 0 {
		return nil
	}

	if len(command.Arguments) == 0 || command.Arguments[0].Type != slashArgumentTypeEnum {
		return fmt.Errorf("slash command %q variants require a leading enum argument", command.Name)
	}

	values := make(map[string]bool, len(command.Arguments[0].Values))
	for _, value := range command.Arguments[0].Values {
		values[value] = true
	}

	for variant := range variants {
		if !values[variant] {
			return fmt.Errorf("slash command %q variant %q is not declared in enum argument %q", command.Name, variant, command.Arguments[0].Name)
		}
	}

	for value := range values {
		if !variants[value] {
			return fmt.Errorf("slash command %q enum value %q is missing a matching variant", command.Name, value)
		}
	}

	return nil
}

func validateSlashCommandEffectsAndPolicy(command *slashCommandDescriptor) error {
	if err := validateSlashMetadataValues(command.Name, command.SideEffects, command.OutputModes, command.PolicyRequirements); err != nil {
		return err
	}

	if slashCommandRequiresPolicy(*command) && len(command.PolicyRequirements) == 0 {
		return fmt.Errorf("slash command %q mutates state or crosses policy boundary without policy requirements", command.Name)
	}

	return nil
}

func validateSlashMetadataValues(commandName string, sideEffects, outputModes, policies []string) error {
	for _, sideEffect := range sideEffects {
		if !isKnownCommandSideEffect(sideEffect) {
			return fmt.Errorf("slash command %q has unknown side effect %q", commandName, sideEffect)
		}
	}

	for _, outputMode := range outputModes {
		if !isKnownCommandOutputMode(outputMode) {
			return fmt.Errorf("slash command %q has unknown output mode %q", commandName, outputMode)
		}
	}

	for _, policy := range policies {
		if !isKnownSlashPolicyRequirement(policy) {
			return fmt.Errorf("slash command %q has unknown policy requirement %q", commandName, policy)
		}
	}

	return nil
}

func slashUsageCommandName(usage string) string {
	fields := strings.Fields(usage)
	if len(fields) == 0 {
		return ""
	}

	return strings.TrimPrefix(strings.ToLower(fields[0]), "/")
}

func slashUsageStartsWithVariant(usage, commandName, variantName string) bool {
	fields := strings.Fields(usage)
	if len(fields) < 2 {
		return false
	}

	return strings.TrimPrefix(strings.ToLower(fields[0]), "/") == commandName &&
		strings.EqualFold(fields[1], variantName)
}

func slashCommandRequiresPolicy(command slashCommandDescriptor) bool {
	return command.MutatesConversation ||
		command.MutatesSessionStore ||
		command.MutatesFilesystem ||
		command.MutatesWorktree ||
		command.RunsLocalProcess ||
		command.UsesClipboard ||
		command.CallsLLM ||
		slashSideEffectsRequirePolicy(command.SideEffects)
}

func slashVariantRequiresPolicy(variant slashCommandVariant) bool {
	return slashSideEffectsRequirePolicy(variant.SideEffects)
}

func slashSideEffectsRequirePolicy(sideEffects []string) bool {
	return slices.Contains(sideEffects, commandEffectSessionWrite) ||
		slices.Contains(sideEffects, commandEffectFilesystemWrite) ||
		slices.Contains(sideEffects, commandEffectWorktreeWrite) ||
		slices.Contains(sideEffects, commandEffectProcessExecute) ||
		slices.Contains(sideEffects, commandEffectLLMProviderRead)
}

func isKnownSlashPolicyRequirement(value string) bool {
	switch value {
	case slashPolicyMutatesConversation,
		slashPolicyMutatesSessionStore,
		slashPolicyMutatesFilesystem,
		slashPolicyMutatesWorktree,
		slashPolicyRunsLocalProcess,
		slashPolicyUsesClipboard,
		slashPolicyCallsLLM:
		return true
	default:
		return false
	}
}

func knownCLICommandContracts() map[string]bool {
	contracts := commandContractsByName()
	inlineContracts := inlineCommandContractsByName()
	out := make(map[string]bool, len(contracts)+len(inlineContracts))

	for name := range contracts {
		out[name] = true
	}

	for name := range inlineContracts {
		out[name] = true
	}

	return out
}

func requiredSlashArg(name, valueType string) slashCommandArgument {
	return slashCommandArgument{Name: name, Type: valueType, Required: true}
}

func optionalSlashArg(name, valueType string) slashCommandArgument {
	return slashCommandArgument{Name: name, Type: valueType}
}

func parseSlashCommandInput(input string) (descriptor slashCommandDescriptor, parsed any, handled bool, err error) {
	if !strings.HasPrefix(strings.TrimSpace(input), "/") {
		return slashCommandDescriptor{}, nil, false, nil
	}

	fields, err := splitSlashCommandFields(input)
	if err != nil {
		return slashCommandDescriptor{}, nil, true, err
	}

	if len(fields) == 0 {
		return slashCommandDescriptor{}, nil, false, nil
	}

	commandName := strings.TrimPrefix(strings.ToLower(fields[0]), "/")

	var ok bool

	descriptor, ok = lookupSlashCommand(commandName)
	if !ok {
		return slashCommandDescriptor{}, nil, true, fmt.Errorf("unknown command: /%s (try /help)", commandName)
	}

	parsed, err = descriptor.Parse(fields[1:])
	if err != nil {
		return descriptor, nil, true, err
	}

	return descriptor, parsed, true, nil
}

func splitSlashCommandFields(input string) ([]string, error) {
	scanner := slashFieldScanner{}
	if err := scanner.scan(input); err != nil {
		return nil, err
	}

	return scanner.fields, nil
}

type slashFieldScanner struct {
	fields       []string
	field        strings.Builder
	quote        rune
	escaping     bool
	fieldStarted bool
}

func (scanner *slashFieldScanner) scan(input string) error {
	for _, r := range input {
		if scanner.consumeEscaped(r) {
			continue
		}

		if r == '\\' {
			scanner.escaping = true
			scanner.fieldStarted = true

			continue
		}

		if scanner.consumeQuoted(r) {
			continue
		}

		if r == '\'' || r == '"' {
			scanner.quote = r
			scanner.fieldStarted = true

			continue
		}

		if unicode.IsSpace(r) {
			scanner.finishField()

			continue
		}

		scanner.writeRune(r)
	}

	if scanner.escaping {
		scanner.field.WriteRune('\\')
	}

	if scanner.quote != 0 {
		return errors.New("unterminated quoted slash command argument")
	}

	scanner.finishField()

	return nil
}

func (scanner *slashFieldScanner) consumeEscaped(r rune) bool {
	if !scanner.escaping {
		return false
	}

	if slashEscapesRune(r) {
		scanner.field.WriteRune(r)
	} else {
		scanner.field.WriteRune('\\')
		scanner.field.WriteRune(r)
	}

	scanner.fieldStarted = true
	scanner.escaping = false

	return true
}

func (scanner *slashFieldScanner) consumeQuoted(r rune) bool {
	if scanner.quote == 0 {
		return false
	}

	if r == scanner.quote {
		scanner.quote = 0

		return true
	}

	scanner.writeRune(r)

	return true
}

func (scanner *slashFieldScanner) finishField() {
	if !scanner.fieldStarted {
		return
	}

	scanner.fields = append(scanner.fields, scanner.field.String())
	scanner.field.Reset()
	scanner.fieldStarted = false
}

func (scanner *slashFieldScanner) writeRune(r rune) {
	scanner.field.WriteRune(r)
	scanner.fieldStarted = true
}

func slashEscapesRune(r rune) bool {
	return unicode.IsSpace(r) || r == '\'' || r == '"' || r == '\\'
}

func lookupSlashCommand(name string) (slashCommandDescriptor, bool) {
	name = strings.TrimPrefix(strings.ToLower(strings.TrimSpace(name)), "/")
	descriptors := slashCommandDescriptors()

	for i := range descriptors {
		descriptor := descriptors[i]
		if descriptor.Name == name || slices.Contains(descriptor.Aliases, name) {
			return descriptor, true
		}
	}

	return slashCommandDescriptor{}, false
}

func slashHelp() string {
	return renderSlashHelp(slashCommandDescriptors())
}

func renderSlashHelp(commands []slashCommandDescriptor) string {
	var out []string

	var line []string

	currentGroup := -1

	for i := range commands {
		command := &commands[i]
		if currentGroup == -1 {
			currentGroup = command.HelpGroup
		}

		if command.HelpGroup != currentGroup {
			out = append(out, strings.Join(line, " "))
			line = nil
			currentGroup = command.HelpGroup
		}

		line = append(line, slashHelpEntries(command)...)
	}

	if len(line) > 0 {
		out = append(out, strings.Join(line, " "))
	}

	return strings.Join(out, "\n")
}

func slashHelpEntries(command *slashCommandDescriptor) []string {
	entries := []string{command.Usage}
	for _, alias := range command.HelpAliases {
		entries = append(entries, "/"+alias)
	}

	return entries
}

func parseNoArgs(args []string, usage string) (slashNoArgsInput, error) {
	if len(args) > 0 {
		return slashNoArgsInput{}, slashUsageError(usage)
	}

	return slashNoArgsInput{}, nil
}

func parseOptionalValue(args []string, usage string) (slashOptionalValueInput, error) {
	if len(args) > 1 {
		return slashOptionalValueInput{}, slashUsageError(usage)
	}

	if len(args) == 0 {
		return slashOptionalValueInput{}, nil
	}

	return slashOptionalValueInput{Value: args[0]}, nil
}

func parseForkInput(args []string, usage string) (slashForkInput, error) {
	if len(args) == 0 {
		return slashForkInput{}, nil
	}

	if len(args) > 1 {
		return slashForkInput{}, slashUsageError(usage)
	}

	count, err := strconv.Atoi(args[0])
	if err != nil {
		return slashForkInput{}, errors.New("fork count must be a number")
	}

	return slashForkInput{Count: count, HasCount: true}, nil
}

func parseSearchInput(args []string, usage string) (slashSearchInput, error) {
	if len(args) == 0 {
		return slashSearchInput{}, slashUsageError(usage)
	}

	return slashSearchInput{Query: strings.Join(args, " ")}, nil
}

func parseMessageNumberInput(args []string, usage string) (slashMessageNumberInput, error) {
	if len(args) != 1 {
		return slashMessageNumberInput{}, slashUsageError(usage)
	}

	number, err := strconv.Atoi(args[0])
	if err != nil {
		return slashMessageNumberInput{}, errors.New("invalid message number")
	}

	return slashMessageNumberInput{Number: number}, nil
}

func parseContextInput(args []string, usage string) (slashContextInput, error) {
	if len(args) == 0 {
		return slashContextInput{}, nil
	}

	if len(args) == 1 && args[0] == "prune" {
		return slashContextInput{Prune: true}, nil
	}

	return slashContextInput{}, slashUsageError(usage)
}

func parseModeInput(args []string, usage string) (slashModeInput, error) {
	if len(args) == 0 {
		return slashModeInput{Show: true}, nil
	}

	if len(args) != 1 {
		return slashModeInput{}, slashUsageError(usage)
	}

	if args[0] != "plan" && args[0] != "execute" {
		return slashModeInput{}, errors.New("mode must be plan or execute")
	}

	return slashModeInput{Mode: args[0]}, nil
}

func parseSuggestionsInput(args []string, usage string) (slashSuggestionsInput, error) {
	if len(args) == 0 {
		return slashSuggestionsInput{Show: true}, nil
	}

	if len(args) != 1 {
		return slashSuggestionsInput{}, slashUsageError(usage)
	}

	mode := strings.ToLower(strings.TrimSpace(args[0]))
	switch mode {
	case "status", "show":
		return slashSuggestionsInput{Show: true}, nil
	case "local", string(promptSuggestionConsentLocalOnly), "disable", "no-network", "offline":
		return slashSuggestionsInput{Mode: string(promptSuggestionConsentLocalOnly)}, nil
	case string(promptSuggestionConsentSession), "session-only":
		return slashSuggestionsInput{Mode: string(promptSuggestionConsentSession)}, nil
	case string(promptSuggestionConsentFolder):
		return slashSuggestionsInput{Mode: string(promptSuggestionConsentFolder)}, nil
	case string(promptSuggestionConsentGlobal):
		return slashSuggestionsInput{Mode: string(promptSuggestionConsentGlobal)}, nil
	default:
		return slashSuggestionsInput{}, slashUsageError(usage)
	}
}

func parseSaveCodeInput(args []string, usage string) (slashSaveCodeInput, error) {
	if len(args) != 2 {
		return slashSaveCodeInput{}, slashUsageError(usage)
	}

	block, err := strconv.Atoi(args[0])
	if err != nil {
		return slashSaveCodeInput{}, errors.New("invalid code block")
	}

	return slashSaveCodeInput{Block: block, Path: args[1]}, nil
}

func parseCopyInput(args []string, usage string) (slashCopyInput, error) {
	if len(args) == 0 {
		return slashCopyInput{Target: "last"}, nil
	}

	if len(args) != 1 || (args[0] != "last" && args[0] != sessionCommandName) {
		return slashCopyInput{}, slashUsageError(usage)
	}

	return slashCopyInput{Target: args[0]}, nil
}

func parseCopyCodeInput(args []string, usage string) (slashCopyCodeInput, error) {
	if len(args) == 0 {
		return slashCopyCodeInput{Block: 1}, nil
	}

	if len(args) != 1 {
		return slashCopyCodeInput{}, slashUsageError(usage)
	}

	block, err := strconv.Atoi(args[0])
	if err != nil {
		return slashCopyCodeInput{}, errors.New("invalid code block")
	}

	return slashCopyCodeInput{Block: block}, nil
}

func parseEvalInput(args []string, usage string) (slashEvalInput, error) {
	if len(args) != 1 || (args[0] != "add" && args[0] != "run") {
		return slashEvalInput{}, slashUsageError(usage)
	}

	return slashEvalInput{Action: args[0]}, nil
}

func slashUsageError(usage string) error {
	return fmt.Errorf("usage: %s", usage)
}

func mergedSlashSideEffects(descriptor slashCommandDescriptor) []string {
	return mergedSlashMetadata(descriptor.SharedCLICommands, descriptorLocalAndVariantSideEffects(descriptor), func(contract commandContract) []string {
		return contract.SideEffects
	})
}

func mergedSlashOutputModes(descriptor slashCommandDescriptor) []string {
	return mergedSlashMetadata(descriptor.SharedCLICommands, descriptorLocalAndVariantOutputModes(descriptor), func(contract commandContract) []string {
		return contract.OutputModes
	})
}

func descriptorLocalAndVariantSideEffects(descriptor slashCommandDescriptor) []string {
	out := append([]string(nil), descriptor.SideEffects...)
	for i := range descriptor.Variants {
		out = append(out, descriptor.Variants[i].SideEffects...)
	}

	return out
}

func descriptorLocalAndVariantOutputModes(descriptor slashCommandDescriptor) []string {
	out := append([]string(nil), descriptor.OutputModes...)
	for i := range descriptor.Variants {
		out = append(out, descriptor.Variants[i].OutputModes...)
	}

	return out
}

func mergedSlashMetadata(
	sharedCommands []string,
	localValues []string,
	contractValues func(commandContract) []string,
) []string {
	out := make([]string, 0, len(localValues))
	seen := make(map[string]bool)
	contracts := commandContractsByName()
	inlineContracts := inlineCommandContractsByName()

	for _, commandName := range sharedCommands {
		if contract, ok := contracts[commandName]; ok {
			out = appendUniqueStrings(out, seen, contractValues(contract))
			continue
		}

		if contract, ok := inlineContracts[commandName]; ok {
			out = appendUniqueStrings(out, seen, contractValues(contract))
		}
	}

	return appendUniqueStrings(out, seen, localValues)
}

func mergedSlashPolicyRequirements(descriptor slashCommandDescriptor) []string {
	out := make([]string, 0, len(descriptor.PolicyRequirements)+4)
	seen := make(map[string]bool)
	out = appendUniqueStrings(out, seen, descriptor.PolicyRequirements)

	for i := range descriptor.Variants {
		out = appendUniqueStrings(out, seen, descriptor.Variants[i].PolicyRequirements)
	}

	if descriptor.MutatesConversation {
		out = appendUniqueStrings(out, seen, []string{slashPolicyMutatesConversation})
	}

	if descriptor.MutatesSessionStore {
		out = appendUniqueStrings(out, seen, []string{slashPolicyMutatesSessionStore})
	}

	if descriptor.MutatesFilesystem {
		out = appendUniqueStrings(out, seen, []string{slashPolicyMutatesFilesystem})
	}

	if descriptor.MutatesWorktree {
		out = appendUniqueStrings(out, seen, []string{slashPolicyMutatesWorktree})
	}

	if descriptor.RunsLocalProcess {
		out = appendUniqueStrings(out, seen, []string{slashPolicyRunsLocalProcess})
	}

	if descriptor.UsesClipboard {
		out = appendUniqueStrings(out, seen, []string{slashPolicyUsesClipboard})
	}

	if descriptor.CallsLLM {
		out = appendUniqueStrings(out, seen, []string{slashPolicyCallsLLM})
	}

	return out
}
