package main

import (
	"context"
	"fmt"
	"io"
	"strings"
)

type codeIntelCommand struct {
	match func(codeIntelCommandInput) bool
	name  string
}

func providerlessConfigCodeIntelCommands() []command {
	return []command{
		{
			name:  codeIntelDomainName,
			tier:  tierProviderlessConfig,
			match: codeIntelCommandRequested,
			runProviderlessConfig: func(ctx context.Context, o cliOptions, s appState) error {
				return runCodeIntelCommand(ctx, s.cwd, codeIntelCommandInputFromOptions(o))
			},
		},
	}
}

func codeIntelCommandRequested(opts cliOptions) bool {
	return matchingCodeIntelCommand(codeIntelCommandInputFromOptions(opts)) != nil
}

func runCodeIntelCommand(ctx context.Context, cwd string, input codeIntelCommandInput) error {
	return runCodeIntelCommandWithWriter(ctx, nil, cwd, input)
}

func runCodeIntelCommandWithWriter(ctx context.Context, w io.Writer, cwd string, input codeIntelCommandInput) error {
	cmd, err := selectCodeIntelCommand(input)
	if err != nil {
		return err
	}

	if cmd == nil {
		return nil
	}

	if w != nil {
		return runCodeIntelSchemaCommandWithWriter(ctx, w, cwd, input, cmd.name)
	}

	return runCodeIntelSchemaCommand(ctx, cwd, input, cmd.name)
}

func matchingCodeIntelCommand(input codeIntelCommandInput) *codeIntelCommand {
	matches := matchingCodeIntelCommands(input)
	if len(matches) == 0 {
		return nil
	}

	return matches[0]
}

func selectCodeIntelCommand(input codeIntelCommandInput) (*codeIntelCommand, error) {
	matches := matchingCodeIntelCommands(input)
	switch len(matches) {
	case 0:
		return nil, nil
	case 1:
		return matches[0], nil
	default:
		return nil, fmt.Errorf("ambiguous CLI command: flags match multiple code-intel commands (%s); choose one command or remove conflicting flags",
			codeIntelCommandNames(matches))
	}
}

func matchingCodeIntelCommands(input codeIntelCommandInput) []*codeIntelCommand {
	matches := make([]*codeIntelCommand, 0, 1)
	commands := codeIntelCommands()

	for i := range commands {
		cmd := &commands[i]
		if cmd.match(input) {
			matches = append(matches, cmd)
		}
	}

	return matches
}

func codeIntelCommandNames(commands []*codeIntelCommand) string {
	names := make([]string, 0, len(commands))
	for _, command := range commands {
		names = append(names, command.name)
	}

	return strings.Join(names, ", ")
}

func codeIntelCommands() []codeIntelCommand {
	descriptors := codeIntelCommandDescriptors()

	commands := make([]codeIntelCommand, 0, len(descriptors))
	for _, descriptor := range descriptors {
		commands = append(commands, codeIntelCommand{name: descriptor.Name, match: descriptor.Match})
	}

	return commands
}
