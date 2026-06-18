package main

import (
	"flag"
	"fmt"
	"sort"
	"strings"

	"github.com/tommoulard/atteler/pkg/llm"
)

func registeredCLIFlagSummaries() []string {
	fs := flag.NewFlagSet("atteler-autopilot-surface", flag.ContinueOnError)

	var opts cliOptions

	initCLIFlagValues(&opts)
	registerCLIFlagsWithFlagSet(fs, &opts)

	out := make([]string, 0, fs.NFlag())
	fs.VisitAll(func(f *flag.Flag) {
		usage := strings.TrimSpace(f.Usage)
		if usage == "" {
			out = append(out, "--"+f.Name)

			return
		}

		out = append(out, "--"+f.Name+": "+usage)
	})
	sort.Strings(out)

	return out
}

func commandSurfaceSummaries(domains []commandSurfaceDomain) []string {
	var out []string

	for i := range domains {
		domain := domains[i]

		for j := range domain.Commands {
			out = append(out, formatDomainCommandSummary(domain.Name, domain.Commands[j]))
		}

		for j := range domain.RoutingCommands {
			out = append(out, formatDomainCommandSummary(domain.Name, domain.RoutingCommands[j]))
		}
	}

	sort.Strings(out)

	return out
}

func formatDomainCommandSummary(domain string, command commandSurfaceDomainCommand) string {
	name := "atteler " + domain + " " + command.Name
	if command.Args != "" {
		name += " " + command.Args
	}

	if command.Summary == "" {
		return name
	}

	return name + ": " + command.Summary
}

func slashCommandSurfaceSummaries(commands []commandSurfaceSlashCommand) []string {
	out := make([]string, 0, len(commands))
	for i := range commands {
		command := commands[i]

		usage := strings.TrimSpace(command.Usage)
		if usage == "" {
			usage = command.Name
		}

		if command.Summary == "" {
			out = append(out, usage)
			continue
		}

		out = append(out, usage+": "+command.Summary)
	}

	sort.Strings(out)

	return out
}

func toolDefinitionSummaries(tools []llm.ToolDefinition) []string {
	out := make([]string, 0, len(tools))
	for i := range tools {
		tool := tools[i]
		if strings.TrimSpace(tool.Description) == "" {
			out = append(out, tool.Name)
			continue
		}

		out = append(out, fmt.Sprintf("%s: %s", tool.Name, strings.TrimSpace(tool.Description)))
	}

	sort.Strings(out)

	return out
}
