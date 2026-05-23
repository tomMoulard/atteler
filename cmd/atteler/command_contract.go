package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"
	"strings"
)

const (
	commandSurfaceSchema       = "atteler.cli.command-surface.v1"
	attelerCommandName         = "atteler"
	commandTierInline          = "inline"
	genericCLIOptionsInputType = "cliOptions"
)

const (
	commandConflictExclusive = "exclusive-command"
	commandConflictOneOf     = "one-of-flags"
	commandConflictOrdered   = "explicit-precedence"
)

const (
	commandEffectConfigRead      = "config-read"
	commandEffectFilesystemRead  = "filesystem-read"
	commandEffectFilesystemWrite = "filesystem-write"
	commandEffectGitRead         = "git-read"
	commandEffectLLMProviderRead = "llm-provider-read"
	commandEffectProcessExecute  = "process-execute"
	commandEffectSessionRead     = "session-store-read"
	commandEffectSessionWrite    = "session-store-write"
	commandEffectStateRead       = "state-store-read"
	commandEffectTaskWrite       = "task-store-write"
	commandEffectUserOutput      = "stdout"
	commandEffectWorktreeWrite   = "worktree-write"
)

const (
	commandOutputFilesystem = "filesystem"
	commandOutputJSON       = "json"
	commandOutputMarkdown   = "markdown"
	commandOutputProcess    = "process"
	commandOutputText       = "text"
	commandOutputYAML       = "yaml"
)

type commandContract struct {
	Summary          string                `json:"summary"`
	InputType        string                `json:"input_type"`
	InputFlags       []string              `json:"input_flags"`
	ConflictRules    []commandConflictRule `json:"conflict_rules"`
	Examples         []string              `json:"examples"`
	SideEffects      []string              `json:"side_effects"`
	OutputModes      []string              `json:"output_modes"`
	Fixtures         []commandFixture      `json:"fixtures"`
	Overrides        []string              `json:"overrides,omitempty"`
	CodeIntelQueries []codeIntelQueryDoc   `json:"code_intel_queries,omitempty"`
}

//nolint:govet // JSON readability matters more than pointer-byte packing.
type commandConflictRule struct {
	With   []string `json:"with,omitempty"`
	Kind   string   `json:"kind"`
	Reason string   `json:"reason"`
}

//nolint:govet // JSON readability matters more than pointer-byte packing.
type commandFixture struct {
	Args        []string `json:"args"`
	Name        string   `json:"name"`
	WantCommand string   `json:"want_command"`
}

type commandSurface struct {
	Schema        string                       `json:"schema"`
	Domains       []commandSurfaceDomain       `json:"domains"`
	Commands      []commandSurfaceCommand      `json:"commands"`
	SlashCommands []commandSurfaceSlashCommand `json:"slash_commands"`
}

type commandSurfaceDomain struct {
	Name            string                        `json:"name"`
	Title           string                        `json:"title"`
	Summary         string                        `json:"summary"`
	Aliases         []string                      `json:"aliases,omitempty"`
	Commands        []commandSurfaceDomainCommand `json:"commands"`
	RoutingCommands []commandSurfaceDomainCommand `json:"routing_commands,omitempty"`
	Examples        []string                      `json:"examples,omitempty"`
}

type commandSurfaceDomainCommand struct {
	Name             string   `json:"name"`
	Summary          string   `json:"summary"`
	Args             string   `json:"args,omitempty"`
	TextOutput       string   `json:"text_output,omitempty"`
	JSONSchema       string   `json:"json_schema,omitempty"`
	Aliases          []string `json:"aliases,omitempty"`
	Examples         []string `json:"examples,omitempty"`
	LegacyFlags      []string `json:"legacy_flags,omitempty"`
	DispatchCommands []string `json:"dispatch_commands,omitempty"`
	SideEffects      []string `json:"side_effects,omitempty"`
	OutputModes      []string `json:"output_modes,omitempty"`
	JSONFields       []string `json:"json_fields,omitempty"`
	JoinArgs         bool     `json:"join_args,omitempty"`
	PromptAfterValue bool     `json:"prompt_after_value,omitempty"`
	PromptFromStdin  bool     `json:"prompt_from_stdin,omitempty"`
	OpaqueArgs       bool     `json:"opaque_args,omitempty"`
}

type commandSurfaceCommand struct {
	Name             string                `json:"name"`
	Tier             string                `json:"tier"`
	Summary          string                `json:"summary"`
	InputType        string                `json:"input_type"`
	InputFields      []string              `json:"input_fields,omitempty"`
	InputFlags       []string              `json:"input_flags"`
	ConflictRules    []commandConflictRule `json:"conflict_rules"`
	Examples         []string              `json:"examples"`
	SideEffects      []string              `json:"side_effects"`
	OutputModes      []string              `json:"output_modes"`
	Fixtures         []commandFixture      `json:"fixtures"`
	Overrides        []string              `json:"overrides,omitempty"`
	CodeIntelQueries []codeIntelQueryDoc   `json:"code_intel_queries,omitempty"`
}

type commandSurfaceSlashCommand struct {
	Name               string                 `json:"name"`
	Usage              string                 `json:"usage"`
	Summary            string                 `json:"summary"`
	InputType          string                 `json:"input_type"`
	InputFields        []string               `json:"input_fields,omitempty"`
	Arguments          []slashCommandArgument `json:"arguments,omitempty"`
	Aliases            []string               `json:"aliases,omitempty"`
	HelpAliases        []string               `json:"help_aliases,omitempty"`
	SharedCLICommands  []string               `json:"shared_cli_commands,omitempty"`
	Variants           []slashCommandVariant  `json:"variants,omitempty"`
	CompletionTokens   []string               `json:"completion_tokens,omitempty"`
	SideEffects        []string               `json:"side_effects"`
	OutputModes        []string               `json:"output_modes"`
	PolicyRequirements []string               `json:"policy_requirements,omitempty"`
	HelpGroup          int                    `json:"help_group"`
}

func commandContractFor(
	summary string,
	inputFlags []string,
	sideEffects []string,
	outputModes []string,
	options ...func(*commandContract),
) commandContract {
	contract := commandContract{
		Summary:       summary,
		InputType:     genericCLIOptionsInputType,
		InputFlags:    normalizeContractList(inputFlags),
		ConflictRules: exclusiveCommandConflictRules(),
		Examples:      []string{legacyFlagExample(inputFlags)},
		SideEffects:   normalizeContractList(sideEffects),
		OutputModes:   normalizeContractList(outputModes),
		Fixtures: []commandFixture{
			{Name: "legacy-flag", Args: legacyFlagFixtureArgs(inputFlags)},
		},
	}

	for _, option := range options {
		option(&contract)
	}

	return contract
}

func withInputType(inputType string) func(*commandContract) {
	return func(contract *commandContract) {
		contract.InputType = inputType
	}
}

func withConflictRule(rule commandConflictRule) func(*commandContract) {
	return func(contract *commandContract) {
		contract.ConflictRules = append(contract.ConflictRules, rule)
	}
}

func withOverrides(names ...string) func(*commandContract) {
	return func(contract *commandContract) {
		contract.Overrides = normalizeContractList(names)
		if len(names) == 0 {
			return
		}

		contract.ConflictRules = append(contract.ConflictRules, commandConflictRule{
			Kind:   commandConflictOrdered,
			With:   normalizeContractList(names),
			Reason: "this command intentionally wins when these supplemental flags are also present",
		})
	}
}

func withExamples(examples ...string) func(*commandContract) {
	return func(contract *commandContract) {
		contract.Examples = normalizeContractList(examples)
	}
}

func withCodeIntelQueries(queries []codeIntelQueryDoc) func(*commandContract) {
	return func(contract *commandContract) {
		contract.CodeIntelQueries = append([]codeIntelQueryDoc(nil), queries...)
	}
}

func attachCommandContracts(registry []command) {
	contracts := commandContractsByName()
	for i := range registry {
		contract, ok := contracts[registry[i].name]
		if !ok {
			panic("missing CLI command contract for " + registry[i].name)
		}

		contract.fillDerivedFields(registry[i].name)
		registry[i].contract = contract
	}

	mustValidateCommandContracts(registry)
}

func (contract *commandContract) fillDerivedFields(commandName string) {
	for i := range contract.Fixtures {
		if contract.Fixtures[i].WantCommand == "" {
			contract.Fixtures[i].WantCommand = commandName
		}
	}
}

func (contract *commandContract) coversMatchedCommands(candidateName string, matches []*command) bool {
	if len(contract.Overrides) == 0 {
		return false
	}

	overrides := make(map[string]bool, len(contract.Overrides))
	for _, name := range contract.Overrides {
		overrides[name] = true
	}

	for _, match := range matches {
		if match.name == candidateName {
			continue
		}

		if !overrides[match.name] {
			return false
		}
	}

	return true
}

func mustValidateCommandContracts(commands []command) {
	if err := validateCommandContracts(commands); err != nil {
		panic(err)
	}
}

func validateCommandContracts(commands []command) error {
	names := make(map[string]bool, len(commands))

	for i := range commands {
		if strings.TrimSpace(commands[i].name) == "" {
			return errors.New("command registry entry missing name")
		}

		if names[commands[i].name] {
			return fmt.Errorf("duplicate command registry entry %q", commands[i].name)
		}

		names[commands[i].name] = true

		if err := validateCommandContract(commands[i].name, commands[i].contract); err != nil {
			return err
		}
	}

	return validateCommandOverrides(commands, names)
}

func validateCommandContract(commandName string, contract commandContract) error {
	if err := validateCommandContractRequiredFields(commandName, contract); err != nil {
		return err
	}

	if err := validateCommandInputType(commandName, contract.InputType); err != nil {
		return err
	}

	if err := validateCommandConflictRules(commandName, contract.ConflictRules); err != nil {
		return err
	}

	if err := validateCommandSideEffects(commandName, contract.SideEffects); err != nil {
		return err
	}

	if err := validateCommandOutputModes(commandName, contract.OutputModes); err != nil {
		return err
	}

	return validateCommandFixtures(commandName, contract.Fixtures)
}

func validateCommandOverrides(commands []command, names map[string]bool) error {
	for i := range commands {
		for _, target := range commands[i].contract.Overrides {
			if !names[target] {
				return fmt.Errorf("command %q override target %q is not registered", commands[i].name, target)
			}
		}
	}

	return nil
}

func validateCommandContractRequiredFields(commandName string, contract commandContract) error {
	switch {
	case strings.TrimSpace(contract.Summary) == "":
		return fmt.Errorf("command %q contract missing summary", commandName)
	case strings.TrimSpace(contract.InputType) == "":
		return fmt.Errorf("command %q contract missing input type", commandName)
	case contract.InputType == genericCLIOptionsInputType:
		return fmt.Errorf("command %q contract must expose a command-specific input type", commandName)
	case len(contract.InputFlags) == 0:
		return fmt.Errorf("command %q contract missing input flags", commandName)
	case len(contract.ConflictRules) == 0:
		return fmt.Errorf("command %q contract missing conflict rules", commandName)
	case len(contract.Examples) == 0:
		return fmt.Errorf("command %q contract missing examples", commandName)
	case len(contract.SideEffects) == 0:
		return fmt.Errorf("command %q contract missing side effects", commandName)
	case len(contract.OutputModes) == 0:
		return fmt.Errorf("command %q contract missing output modes", commandName)
	case len(contract.Fixtures) == 0:
		return fmt.Errorf("command %q contract missing fixtures", commandName)
	}

	return nil
}

func validateCommandInputType(commandName, inputType string) error {
	builder, ok := commandInputBuildersByType()[inputType]
	if !ok {
		return fmt.Errorf("command %q contract references unknown input type %q", commandName, inputType)
	}

	inputValue := reflect.TypeOf(builder(cliOptions{}))
	if inputValue == nil {
		return fmt.Errorf("command %q contract input builder %q returned nil", commandName, inputType)
	}

	if inputValue.Kind() == reflect.Pointer {
		inputValue = inputValue.Elem()
	}

	if inputValue.Kind() != reflect.Struct {
		return fmt.Errorf("command %q contract input type %q must be a struct", commandName, inputType)
	}

	return nil
}

func validateCommandConflictRules(commandName string, rules []commandConflictRule) error {
	for i := range rules {
		if err := validateCommandConflictRule(commandName, rules[i]); err != nil {
			return err
		}
	}

	return nil
}

func validateCommandSideEffects(commandName string, sideEffects []string) error {
	for _, sideEffect := range sideEffects {
		if !isKnownCommandSideEffect(sideEffect) {
			return fmt.Errorf("command %q contract has unknown side effect %q", commandName, sideEffect)
		}
	}

	return nil
}

func validateCommandOutputModes(commandName string, outputModes []string) error {
	for _, outputMode := range outputModes {
		if !isKnownCommandOutputMode(outputMode) {
			return fmt.Errorf("command %q contract has unknown output mode %q", commandName, outputMode)
		}
	}

	return nil
}

func validateCommandFixtures(commandName string, fixtures []commandFixture) error {
	for i := range fixtures {
		if err := validateCommandFixture(commandName, fixtures[i]); err != nil {
			return err
		}
	}

	return nil
}

func validateCommandConflictRule(commandName string, rule commandConflictRule) error {
	if !isKnownCommandConflictKind(rule.Kind) {
		return fmt.Errorf("command %q contract has unknown conflict kind %q", commandName, rule.Kind)
	}

	if strings.TrimSpace(rule.Reason) == "" {
		return fmt.Errorf("command %q contract conflict %q missing reason", commandName, rule.Kind)
	}

	return nil
}

func validateCommandFixture(commandName string, fixture commandFixture) error {
	if strings.TrimSpace(fixture.Name) == "" {
		return fmt.Errorf("command %q contract fixture missing name", commandName)
	}

	if len(fixture.Args) == 0 {
		return fmt.Errorf("command %q contract fixture %q missing args", commandName, fixture.Name)
	}

	if fixture.WantCommand != commandName {
		return fmt.Errorf("command %q contract fixture %q wants %q", commandName, fixture.Name, fixture.WantCommand)
	}

	return nil
}

func isKnownCommandConflictKind(value string) bool {
	switch value {
	case commandConflictExclusive,
		commandConflictOneOf,
		commandConflictOrdered:
		return true
	default:
		return false
	}
}

func isKnownCommandSideEffect(value string) bool {
	switch value {
	case commandEffectConfigRead,
		commandEffectFilesystemRead,
		commandEffectFilesystemWrite,
		commandEffectGitRead,
		commandEffectLLMProviderRead,
		commandEffectProcessExecute,
		commandEffectSessionRead,
		commandEffectSessionWrite,
		commandEffectStateRead,
		commandEffectTaskWrite,
		commandEffectUserOutput,
		commandEffectWorktreeWrite:
		return true
	default:
		return false
	}
}

func isKnownCommandOutputMode(value string) bool {
	switch value {
	case commandOutputFilesystem,
		commandOutputJSON,
		commandOutputMarkdown,
		commandOutputProcess,
		commandOutputText,
		commandOutputYAML:
		return true
	default:
		return false
	}
}

func buildCommandSurface(registry []command) commandSurface {
	inlineCommands := buildInlineCommandRegistry()
	commands := make([]commandSurfaceCommand, 0, len(registry)+len(inlineCommands))

	for i := range registry {
		cmd := &registry[i]
		commands = append(commands, commandSurfaceCommand{
			Name:          cmd.name,
			Tier:          cmd.tier.String(),
			Summary:       cmd.contract.Summary,
			InputType:     cmd.contract.InputType,
			InputFields:   commandInputFieldNames(cmd.contract.InputType),
			InputFlags:    append([]string(nil), cmd.contract.InputFlags...),
			ConflictRules: append([]commandConflictRule(nil), cmd.contract.ConflictRules...),
			Examples:      append([]string(nil), cmd.contract.Examples...),
			SideEffects:   append([]string(nil), cmd.contract.SideEffects...),
			OutputModes:   append([]string(nil), cmd.contract.OutputModes...),
			Fixtures:      append([]commandFixture(nil), cmd.contract.Fixtures...),
			Overrides:     append([]string(nil), cmd.contract.Overrides...),
			CodeIntelQueries: append([]codeIntelQueryDoc(nil),
				cmd.contract.CodeIntelQueries...),
		})
	}

	for i := range inlineCommands {
		cmd := &inlineCommands[i]
		commands = append(commands, commandSurfaceCommand{
			Name:          cmd.name,
			Tier:          cmd.tier.String(),
			Summary:       cmd.contract.Summary,
			InputType:     cmd.contract.InputType,
			InputFields:   commandInputFieldNames(cmd.contract.InputType),
			InputFlags:    append([]string(nil), cmd.contract.InputFlags...),
			ConflictRules: append([]commandConflictRule(nil), cmd.contract.ConflictRules...),
			Examples:      append([]string(nil), cmd.contract.Examples...),
			SideEffects:   append([]string(nil), cmd.contract.SideEffects...),
			OutputModes:   append([]string(nil), cmd.contract.OutputModes...),
			Fixtures:      append([]commandFixture(nil), cmd.contract.Fixtures...),
			Overrides:     append([]string(nil), cmd.contract.Overrides...),
			CodeIntelQueries: append([]codeIntelQueryDoc(nil),
				cmd.contract.CodeIntelQueries...),
		})
	}

	return commandSurface{
		Schema:        commandSurfaceSchema,
		Domains:       commandSurfaceDomains(commands),
		Commands:      commands,
		SlashCommands: commandSurfaceSlashCommands(),
	}
}

func commandSurfaceSlashCommands() []commandSurfaceSlashCommand {
	descriptors := slashCommandDescriptors()
	out := make([]commandSurfaceSlashCommand, 0, len(descriptors))

	for i := range descriptors {
		descriptor := &descriptors[i]
		out = append(out, commandSurfaceSlashCommand{
			Name:               descriptor.Name,
			Usage:              descriptor.Usage,
			Summary:            descriptor.Summary,
			InputType:          descriptor.InputType,
			InputFields:        append([]string(nil), descriptor.InputFields...),
			Arguments:          copySlashArguments(descriptor.Arguments),
			Aliases:            append([]string(nil), descriptor.Aliases...),
			HelpAliases:        append([]string(nil), descriptor.HelpAliases...),
			SharedCLICommands:  append([]string(nil), descriptor.SharedCLICommands...),
			Variants:           copySlashVariants(descriptor.Variants),
			CompletionTokens:   append([]string(nil), descriptor.CompletionTokens...),
			SideEffects:        append([]string(nil), descriptor.SideEffects...),
			OutputModes:        append([]string(nil), descriptor.OutputModes...),
			PolicyRequirements: append([]string(nil), descriptor.PolicyRequirements...),
			HelpGroup:          descriptor.HelpGroup,
		})
	}

	return out
}

func commandInputFieldNames(inputType string) []string {
	if inputType == "" || inputType == genericCLIOptionsInputType {
		return nil
	}

	builder, ok := commandInputBuildersByType()[inputType]
	if !ok {
		return nil
	}

	inputTypeValue := reflect.TypeOf(builder(cliOptions{}))
	if inputTypeValue == nil {
		return nil
	}

	if inputTypeValue.Kind() == reflect.Pointer {
		inputTypeValue = inputTypeValue.Elem()
	}

	if inputTypeValue.Kind() != reflect.Struct {
		return nil
	}

	fields := make([]string, 0, inputTypeValue.NumField())
	for field := range inputTypeValue.Fields() {
		if field.PkgPath == "" {
			fields = append(fields, field.Name)
		}
	}

	return fields
}

func commandSurfaceDomains(commands []commandSurfaceCommand) []commandSurfaceDomain {
	domains := make([]commandSurfaceDomain, 0, len(cliHelpDomains))
	for i := range cliHelpDomains {
		domain := &cliHelpDomains[i]
		domains = append(domains, commandSurfaceDomain{
			Name:            domain.Name,
			Title:           domain.Title,
			Summary:         domain.Summary,
			Aliases:         append([]string(nil), domain.Aliases...),
			Commands:        commandSurfaceDomainCommands(domain.Commands, commands),
			RoutingCommands: commandSurfaceDomainCommands(domainRoutingOnlyCommands(domain), commands),
			Examples:        append([]string(nil), domain.Examples...),
		})
	}

	return domains
}

func domainRoutingOnlyCommands(domain *cliHelpDomain) []cliCommandAlias {
	if len(domain.RoutingCommands) == 0 {
		return nil
	}

	visible := make(map[string]struct{}, len(domain.Commands))
	for i := range domain.Commands {
		visible[normalizeHelpName(domain.Commands[i].Name)] = struct{}{}
	}

	routingOnly := make([]cliCommandAlias, 0, len(domain.RoutingCommands))
	for i := range domain.RoutingCommands {
		command := domain.RoutingCommands[i]
		if _, ok := visible[normalizeHelpName(command.Name)]; ok {
			continue
		}

		routingOnly = append(routingOnly, command)
	}

	return routingOnly
}

func commandSurfaceDomainCommands(commands []cliCommandAlias, dispatchCommands []commandSurfaceCommand) []commandSurfaceDomainCommand {
	out := make([]commandSurfaceDomainCommand, 0, len(commands))
	for i := range commands {
		command := &commands[i]
		matches := domainCommandDispatchMatches(command.Legacy, dispatchCommands)
		out = append(out, commandSurfaceDomainCommand{
			Name:             command.Name,
			Summary:          command.Summary,
			Args:             command.Args,
			TextOutput:       command.TextOutput,
			JSONSchema:       command.JSONSchema,
			Aliases:          append([]string(nil), command.Aliases...),
			Examples:         append([]string(nil), command.Examples...),
			LegacyFlags:      append([]string(nil), command.Legacy...),
			DispatchCommands: domainCommandDispatchNames(matches),
			SideEffects:      domainCommandSideEffects(matches),
			OutputModes:      domainCommandOutputModes(matches),
			JSONFields:       append([]string(nil), command.JSONFields...),
			JoinArgs:         command.JoinArgs,
			PromptAfterValue: command.PromptAfterValue,
			PromptFromStdin:  command.PromptFromStdin,
			OpaqueArgs:       command.OpaqueArgs,
		})
	}

	return out
}

func domainCommandDispatchMatches(legacyFlags []string, commands []commandSurfaceCommand) []commandSurfaceCommand {
	if len(legacyFlags) == 0 {
		return nil
	}

	matches := domainCommandDispatchFixtureMatches(legacyFlags, commands)
	if len(matches) > 0 {
		return matches
	}

	return domainCommandDispatchInputFlagMatches(legacyFlags, commands)
}

func domainCommandDispatchFixtureMatches(legacyFlags []string, commands []commandSurfaceCommand) []commandSurfaceCommand {
	legacy := stringSet(legacyFlags)
	matches := make([]commandSurfaceCommand, 0, 1)

	for i := range commands {
		if commandFixturesUseAnyFlag(commands[i].Fixtures, legacy) {
			matches = append(matches, commands[i])
		}
	}

	return matches
}

func commandFixturesUseAnyFlag(fixtures []commandFixture, flags map[string]bool) bool {
	for _, fixture := range fixtures {
		if len(fixture.Args) > 0 && flags[fixture.Args[0]] {
			return true
		}
	}

	return false
}

func domainCommandDispatchInputFlagMatches(legacyFlags []string, commands []commandSurfaceCommand) []commandSurfaceCommand {
	legacy := stringSet(legacyFlags)
	matches := make([]commandSurfaceCommand, 0, 1)

	for i := range commands {
		if commandInputFlagsIncludeAny(commands[i].InputFlags, legacy) {
			matches = append(matches, commands[i])
		}
	}

	return matches
}

func commandInputFlagsIncludeAny(inputFlags []string, flags map[string]bool) bool {
	for _, inputFlag := range inputFlags {
		if flags[inputFlag] {
			return true
		}
	}

	return false
}

func domainCommandDispatchNames(commands []commandSurfaceCommand) []string {
	out := make([]string, 0, len(commands))
	for i := range commands {
		out = append(out, commands[i].Name)
	}

	return out
}

func domainCommandSideEffects(commands []commandSurfaceCommand) []string {
	out := make([]string, 0)
	seen := make(map[string]bool)

	for i := range commands {
		out = appendUniqueStrings(out, seen, commands[i].SideEffects)
	}

	return out
}

func domainCommandOutputModes(commands []commandSurfaceCommand) []string {
	out := make([]string, 0)
	seen := make(map[string]bool)

	for i := range commands {
		out = appendUniqueStrings(out, seen, commands[i].OutputModes)
	}

	return out
}

func appendUniqueStrings(out []string, seen map[string]bool, values []string) []string {
	for _, value := range values {
		if value == "" || seen[value] {
			continue
		}

		seen[value] = true
		out = append(out, value)
	}

	return out
}

func stringSet(values []string) map[string]bool {
	out := make(map[string]bool, len(values))
	for _, value := range values {
		out[value] = true
	}

	return out
}

func printCommandSurfaceJSON(w io.Writer) error {
	out, err := json.MarshalIndent(buildCommandSurface(commandRegistry), "", "  ")
	if err != nil {
		return fmt.Errorf("marshal command surface: %w", err)
	}

	_, err = fmt.Fprintln(w, string(out))
	if err != nil {
		return fmt.Errorf("write command surface: %w", err)
	}

	return nil
}

func printCommandSurfaceMarkdown(w io.Writer) error {
	_, err := fmt.Fprint(w, renderCommandSurfaceMarkdown(buildCommandSurface(commandRegistry)))
	if err != nil {
		return fmt.Errorf("write command surface docs: %w", err)
	}

	return nil
}

func renderCommandSurfaceMarkdown(surface commandSurface) string {
	var out strings.Builder
	out.WriteString("# Atteler command surface\n\n")
	out.WriteString("Schema: `")
	out.WriteString(surface.Schema)
	out.WriteString("`\n\n")

	for domainIndex := range surface.Domains {
		domain := &surface.Domains[domainIndex]

		out.WriteString("## ")
		out.WriteString(domain.Title)
		out.WriteString("\n\n")
		out.WriteString(domain.Summary)
		out.WriteString("\n\n")

		writeMarkdownDomainCommands(&out, "Commands", domain.Commands)
		writeMarkdownDomainCommands(&out, "Routing-only commands", domain.RoutingCommands)

		if len(domain.Examples) > 0 {
			out.WriteString("Examples:\n")

			for _, example := range domain.Examples {
				out.WriteString("- `")
				out.WriteString(example)
				out.WriteString("`\n")
			}

			out.WriteString("\n")
		}
	}

	renderSlashCommandSurfaceMarkdown(&out, surface.SlashCommands)

	out.WriteString("## Dispatch commands\n\n")

	for i := range surface.Commands {
		command := &surface.Commands[i]

		out.WriteString("- `")
		out.WriteString(command.Name)
		out.WriteString("` (")
		out.WriteString(command.Tier)
		out.WriteString("): ")
		out.WriteString(command.Summary)
		out.WriteString("\n")
		writeMarkdownListDetail(&out, "Input", []string{command.InputType})
		writeMarkdownListDetail(&out, "Input fields", command.InputFields)
		writeMarkdownListDetail(&out, "Flags", command.InputFlags)
		writeMarkdownListDetail(&out, "Examples", command.Examples)
		writeMarkdownConflictDetails(&out, command.ConflictRules)
		writeMarkdownListDetail(&out, "Overrides", command.Overrides)
		writeMarkdownListDetail(&out, "Side effects", command.SideEffects)
		writeMarkdownListDetail(&out, "Outputs", command.OutputModes)
		writeMarkdownCodeIntelQueryDetails(&out, command.CodeIntelQueries)
		writeMarkdownFixtureDetails(&out, command.Fixtures)
	}

	return out.String()
}

func renderSlashCommandSurfaceMarkdown(out *strings.Builder, commands []commandSurfaceSlashCommand) {
	if len(commands) == 0 {
		return
	}

	out.WriteString("## Interactive slash commands\n\n")
	out.WriteString("Slash help:\n\n")
	out.WriteString("```text\n")
	out.WriteString(renderCommandSurfaceSlashHelp(commands))
	out.WriteString("\n```\n\n")
	out.WriteString("Commands:\n")

	for i := range commands {
		command := &commands[i]

		out.WriteString("- `")
		out.WriteString(command.Usage)
		out.WriteString("`: ")
		out.WriteString(command.Summary)
		out.WriteString("\n")
		writeMarkdownListDetail(out, "Input", []string{command.InputType})
		writeMarkdownListDetail(out, "Input fields", command.InputFields)
		writeMarkdownListDetail(out, "Arguments", slashArgumentLabels(command.Arguments))
		writeMarkdownListDetail(out, "Aliases", command.Aliases)
		writeMarkdownListDetail(out, "Help aliases", command.HelpAliases)
		writeMarkdownListDetail(out, "Shared CLI commands", command.SharedCLICommands)
		writeMarkdownListDetail(out, "Completion tokens", command.CompletionTokens)
		writeSlashVariantDetails(out, command.Variants)
		writeMarkdownListDetail(out, "Side effects", command.SideEffects)
		writeMarkdownListDetail(out, "Outputs", command.OutputModes)
		writeMarkdownListDetail(out, "Policy", command.PolicyRequirements)
	}

	out.WriteString("\n")
}

func renderCommandSurfaceSlashHelp(commands []commandSurfaceSlashCommand) string {
	var (
		out  []string
		line []string
	)

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

		line = append(line, command.Usage)
		for _, alias := range command.HelpAliases {
			line = append(line, "/"+alias)
		}
	}

	if len(line) > 0 {
		out = append(out, strings.Join(line, " "))
	}

	return strings.Join(out, "\n")
}

func copySlashArguments(arguments []slashCommandArgument) []slashCommandArgument {
	if len(arguments) == 0 {
		return nil
	}

	out := make([]slashCommandArgument, 0, len(arguments))
	for i := range arguments {
		argument := arguments[i]
		argument.Values = append([]string(nil), argument.Values...)
		out = append(out, argument)
	}

	return out
}

func copySlashVariants(variants []slashCommandVariant) []slashCommandVariant {
	if len(variants) == 0 {
		return nil
	}

	out := make([]slashCommandVariant, 0, len(variants))
	for i := range variants {
		variant := variants[i]
		variant.SideEffects = append([]string(nil), variant.SideEffects...)
		variant.OutputModes = append([]string(nil), variant.OutputModes...)
		variant.PolicyRequirements = append([]string(nil), variant.PolicyRequirements...)
		out = append(out, variant)
	}

	return out
}

func slashArgumentLabels(arguments []slashCommandArgument) []string {
	labels := make([]string, 0, len(arguments))
	for i := range arguments {
		argument := &arguments[i]
		label := argument.Name + ":" + argument.Type

		if len(argument.Values) > 0 {
			label += "(" + strings.Join(argument.Values, "|") + ")"
		}

		if argument.Required {
			label += " required"
		} else {
			label += " optional"
		}

		if argument.Variadic {
			label += ", variadic"
		}

		labels = append(labels, label)
	}

	return labels
}

func writeSlashVariantDetails(out *strings.Builder, variants []slashCommandVariant) {
	if len(variants) == 0 {
		return
	}

	out.WriteString("  - Variants:\n")

	for i := range variants {
		variant := &variants[i]

		out.WriteString("    - `")
		out.WriteString(variant.Usage)
		out.WriteString("`: ")
		out.WriteString(variant.Summary)
		out.WriteString("\n")
		writeMarkdownIndentedListDetail(out, "      ", "Side effects", variant.SideEffects)
		writeMarkdownIndentedListDetail(out, "      ", "Outputs", variant.OutputModes)
		writeMarkdownIndentedListDetail(out, "      ", "Policy", variant.PolicyRequirements)
	}
}

func writeMarkdownDomainCommands(out *strings.Builder, title string, commands []commandSurfaceDomainCommand) {
	if len(commands) == 0 {
		return
	}

	out.WriteString(title)
	out.WriteString(":\n")

	for commandIndex := range commands {
		command := &commands[commandIndex]

		out.WriteString("- `")
		out.WriteString(command.Name)

		if command.Args != "" {
			out.WriteString(" ")
			out.WriteString(command.Args)
		}

		out.WriteString("`: ")
		out.WriteString(command.Summary)

		if len(command.DispatchCommands) > 0 {
			out.WriteString(" (dispatch: ")
			writeMarkdownInlineCodeList(out, command.DispatchCommands)
			out.WriteString(")")
		}

		out.WriteString("\n")

		writeDomainCommandOutputDocs(out, command)
	}

	out.WriteString("\n")
}

func writeDomainCommandOutputDocs(out *strings.Builder, command *commandSurfaceDomainCommand) {
	if command.TextOutput != "" {
		writeMarkdownListDetail(out, "Text output", []string{command.TextOutput})
	}

	if command.JSONSchema != "" {
		writeMarkdownListDetail(out, "JSON schema", []string{command.JSONSchema})
	}

	writeMarkdownListDetail(out, "JSON fields", command.JSONFields)
	writeMarkdownListDetail(out, "Examples", command.Examples)
}

func writeMarkdownCodeIntelQueryDetails(out *strings.Builder, queries []codeIntelQueryDoc) {
	if len(queries) == 0 {
		return
	}

	out.WriteString("  - Code-intel queries:\n")

	for i := range queries {
		query := &queries[i]

		out.WriteString("    - `")
		out.WriteString(query.DomainCommand)

		if query.Args != "" {
			out.WriteString(" ")
			out.WriteString(query.Args)
		}

		out.WriteString("` / `")
		out.WriteString(query.LegacyFlag)
		out.WriteString("`: ")
		out.WriteString(query.Summary)
		out.WriteString("\n")
		writeMarkdownIndentedListDetail(out, "      ", "Text output", []string{query.TextOutput})
		writeMarkdownIndentedListDetail(out, "      ", "JSON schema", []string{query.JSONSchema})
		writeMarkdownIndentedListDetail(out, "      ", "JSON fields", query.JSONFields)
		writeMarkdownIndentedListDetail(out, "      ", "Examples", query.Examples)
	}
}

func writeMarkdownListDetail(out *strings.Builder, label string, values []string) {
	writeMarkdownIndentedListDetail(out, "  ", label, values)
}

func writeMarkdownIndentedListDetail(out *strings.Builder, indent, label string, values []string) {
	if len(values) == 0 {
		return
	}

	out.WriteString(indent)
	out.WriteString("- ")
	out.WriteString(label)
	out.WriteString(": ")

	for i, value := range values {
		if i > 0 {
			out.WriteString(", ")
		}

		out.WriteString("`")
		out.WriteString(value)
		out.WriteString("`")
	}

	out.WriteString("\n")
}

func writeMarkdownConflictDetails(out *strings.Builder, rules []commandConflictRule) {
	if len(rules) == 0 {
		return
	}

	out.WriteString("  - Conflicts:\n")

	for _, rule := range rules {
		out.WriteString("    - `")
		out.WriteString(rule.Kind)
		out.WriteString("`")

		if len(rule.With) > 0 {
			out.WriteString(" with ")
			writeMarkdownInlineCodeList(out, rule.With)
		}

		if rule.Reason != "" {
			out.WriteString(": ")
			out.WriteString(rule.Reason)
		}

		out.WriteString("\n")
	}
}

func writeMarkdownFixtureDetails(out *strings.Builder, fixtures []commandFixture) {
	if len(fixtures) == 0 {
		return
	}

	out.WriteString("  - Fixtures:\n")

	for _, fixture := range fixtures {
		out.WriteString("    - `")
		out.WriteString(fixture.Name)
		out.WriteString("`: `atteler")

		for _, arg := range fixture.Args {
			out.WriteString(" ")
			out.WriteString(arg)
		}

		out.WriteString("` -> `")
		out.WriteString(fixture.WantCommand)
		out.WriteString("`\n")
	}
}

func writeMarkdownInlineCodeList(out *strings.Builder, values []string) {
	for i, value := range values {
		if i > 0 {
			out.WriteString(", ")
		}

		out.WriteString("`")
		out.WriteString(value)
		out.WriteString("`")
	}
}

func renderReadmeDomainTable(domains []cliHelpDomain) string {
	var table strings.Builder

	table.WriteString("<!-- atteler:cli-domains:start -->\n")
	table.WriteString("| Domain | Examples |\n")
	table.WriteString("|--------|----------|\n")

	for i := range domains {
		domain := &domains[i]

		table.WriteString("| ")
		table.WriteString(readmeDomainLabel(domain))
		table.WriteString(" | ")
		table.WriteString(readmeExamples(domain.Examples))
		table.WriteString(" |\n")
	}

	table.WriteString("<!-- atteler:cli-domains:end -->")

	return table.String()
}

func readmeDomainLabel(domain *cliHelpDomain) string {
	parts := strings.Split(domain.Name, "/")
	if len(parts) > 1 {
		labels := make([]string, 0, len(parts))
		for _, part := range parts {
			labels = append(labels, "`"+part+"`")
		}

		return strings.Join(labels, " / ")
	}

	return "`" + domain.Name + "`"
}

func readmeExamples(examples []string) string {
	quoted := make([]string, 0, len(examples))
	for _, example := range examples {
		quoted = append(quoted, "`"+example+"`")
	}

	return strings.Join(quoted, ", ")
}

func normalizeContractList(values []string) []string {
	out := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value != "" {
			out = append(out, value)
		}
	}

	return out
}

func exclusiveCommandConflictRules() []commandConflictRule {
	return []commandConflictRule{
		{
			Kind:   commandConflictExclusive,
			With:   []string{"*"},
			Reason: "command-triggering flags are mutually exclusive unless an explicit precedence rule declares otherwise",
		},
	}
}

func legacyFlagExample(inputFlags []string) string {
	args := legacyFlagFixtureArgs(inputFlags)
	if len(args) == 0 {
		return attelerCommandName
	}

	return attelerCommandName + " " + strings.Join(args, " ")
}

func legacyFlagFixtureArgs(inputFlags []string) []string {
	if len(inputFlags) == 0 {
		return nil
	}

	flag := inputFlags[0]
	if flagRequiresFixtureValue(flag) {
		return []string{flag, "value"}
	}

	return []string{flag}
}

func flagRequiresFixtureValue(flag string) bool {
	switch strings.TrimPrefix(flag, "--") {
	case "agent-performance-summary",
		"async-plan",
		"async-run",
		"code-summary",
		"command-surface-docs",
		"command-surface-json",
		"doctor",
		"doctor-offline",
		"explain-config",
		"feedback-proposals",
		"list-agents",
		"list-config-paths",
		"list-headless",
		"list-hook-events",
		"list-known-models",
		"list-models",
		"list-plugins",
		"list-providers",
		"list-session-tags",
		"list-sessions",
		"list-worktrees",
		codeIntelLSPSymbolsName,
		"print-config-template",
		"recover-headless",
		"review-plan",
		"review-run",
		"review-scan",
		"route-interactive",
		"speculate-plan",
		"speculate-run",
		"state-diagnostics",
		"task-list",
		"validate-config",
		"version",
		"watch-loop",
		"watch-scan":
		return false
	default:
		return true
	}
}

func (tier commandTier) String() string {
	switch tier {
	case tierInline:
		return commandTierInline
	case tierProviderless:
		return "providerless"
	case tierProviderlessConfig:
		return "providerless-config"
	case tierStateful:
		return "stateful"
	default:
		return "unknown"
	}
}
