//nolint:wsl_v5 // Help rendering code is easier to scan with compact output blocks.
package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"sort"
	"strconv"
	"strings"
)

const (
	helpSelectorAll           = "all"
	helpSelectorSession       = "session"
	configDomainName          = "config"
	outputFlagName            = "output"
	noLegacyDeprecationNotice = "No legacy flag is deprecated in this release."
)

func groupedUsage() {
	printTopLevelHelp(os.Stderr)
}

func printCLIHelp(w io.Writer, fs *flag.FlagSet, domainName string) error {
	requestedName := strings.TrimSpace(domainName)
	domainName = normalizeHelpName(domainName)

	switch domainName {
	case "", "domains", "overview":
		printTopLevelHelp(w)
		return nil
	case "legacy", "flags", helpSelectorAll:
		printLegacyFlagHelp(w, fs)
		return nil
	default:
		domain, ok := lookupHelpDomain(domainName)
		if !ok {
			if flagDomain, flagOK := helpDomainForFlagSelector(requestedName, fs); flagOK {
				domain, ok = lookupHelpDomain(flagDomain)
			}
		}

		if !ok {
			return fmt.Errorf("unknown help domain %q; run `atteler help` to list domains", domainName)
		}

		printDomainHelp(w, fs, domain, requestedName)
		return nil
	}
}

func printTopLevelHelp(w io.Writer) {
	fmt.Fprintln(w, "Usage:")
	fmt.Fprintln(w, "  atteler [flags] [prompt]")
	fmt.Fprintln(w, "  atteler <domain> <command> [args]")
	fmt.Fprintln(w, "  atteler help [domain]")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Domains:")

	for i := range cliHelpDomains {
		domain := &cliHelpDomains[i]

		fmt.Fprintf(w, "  %-18s %s\n", domain.Name, domain.Summary)
	}

	fmt.Fprintln(w)
	fmt.Fprintln(w, "Examples:")
	fmt.Fprintln(w, `  atteler chat once "Explain this repository"`)
	fmt.Fprintln(w, "  atteler code-intel summary")
	fmt.Fprintln(w, "  atteler review scan")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Existing --flag aliases remain supported for scripts.")
	fmt.Fprintln(w, noLegacyDeprecationNotice)
	fmt.Fprintln(w, "Run `atteler help <domain>` for focused commands and examples.")
	fmt.Fprintln(w, "Run `atteler help legacy` for the full compatibility flag catalog.")
}

func printDomainHelp(w io.Writer, fs *flag.FlagSet, domain cliHelpDomain, requestedName string) {
	fmt.Fprintf(w, "%s\n\n", domain.Title)
	fmt.Fprintln(w, domain.Summary)
	fmt.Fprintln(w)
	usageName := usageNameForDomainSelector(domain, requestedName)
	fmt.Fprintf(w, "Usage: atteler %s <command> [args]\n", usageName)
	fmt.Fprintf(w, "       atteler help %s\n", usageName)

	if len(domain.Aliases) > 0 {
		fmt.Fprintf(w, "\nAliases: %s\n", strings.Join(domain.Aliases, ", "))
	}

	fmt.Fprintln(w, "\nCommands:")
	for i := range domain.Commands {
		command := &domain.Commands[i]
		name := command.Name
		if command.Args != "" {
			name += " " + command.Args
		}

		summary := command.Summary
		if aliases := commandAliasSummary(*command); aliases != "" {
			summary += " (" + aliases + ")"
		}

		printCommandHelpLine(w, name, summary)
	}

	flags := flagNamesForDomain(fs, domain.Name)
	if len(flags) > 0 {
		fmt.Fprintln(w, "\nCompatible legacy flags:")
		printWrappedList(w, flags, "  ", 88)
	}

	if len(domain.Examples) > 0 {
		fmt.Fprintln(w, "\nExamples:")
		for _, example := range domain.Examples {
			fmt.Fprintf(w, "  %s\n", example)
		}
	}

	fmt.Fprintln(w)
	fmt.Fprintln(w, "Compatibility: existing top-level --flag aliases continue to work.")
	fmt.Fprintln(w, noLegacyDeprecationNotice)
}

func printCommandHelpLine(w io.Writer, name, summary string) {
	if len(name) <= 28 {
		fmt.Fprintf(w, "  %-28s %s\n", name, summary)
		return
	}

	fmt.Fprintf(w, "  %s\n  %-28s %s\n", name, "", summary)
}

func usageNameForDomainSelector(domain cliHelpDomain, selector string) string {
	selector = normalizeHelpName(selector)
	if selector == "" {
		return usageNameForDomain(domain)
	}

	if selector == normalizeHelpName(domain.Name) && !strings.Contains(domain.Name, "/") {
		return domain.Name
	}

	for _, alias := range domain.Aliases {
		if selector == normalizeHelpName(alias) {
			if strings.Contains(alias, "/") {
				return usageNameForDomain(domain)
			}

			return alias
		}
	}

	for _, alias := range domain.HiddenAliases {
		if selector == normalizeHelpName(alias) {
			return usageNameForDomain(domain)
		}
	}

	return usageNameForDomain(domain)
}

func legacyAliasSummary(aliases []string) string {
	if len(aliases) == 1 {
		return "alias: " + aliases[0]
	}

	return "aliases: " + strings.Join(aliases, ", ")
}

func commandAliasSummary(command cliCommandAlias) string {
	var parts []string

	if len(command.Aliases) == 1 {
		parts = append(parts, "command alias: "+command.Aliases[0])
	} else if len(command.Aliases) > 1 {
		parts = append(parts, "command aliases: "+strings.Join(command.Aliases, ", "))
	}

	if len(command.Legacy) > 0 {
		parts = append(parts, legacyAliasSummary(command.Legacy))
	}

	return strings.Join(parts, "; ")
}

func usageNameForDomain(domain cliHelpDomain) string {
	if !strings.Contains(domain.Name, "/") {
		return domain.Name
	}

	for _, alias := range domain.Aliases {
		if alias != "" {
			return alias
		}
	}

	return domain.Name
}

func printLegacyFlagHelp(w io.Writer, fs *flag.FlagSet) {
	fmt.Fprintln(w, "Usage: atteler [flags] [prompt]")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Compatibility flag catalog:")
	fmt.Fprintln(w, noLegacyDeprecationNotice)
	fmt.Fprintln(w)

	groups := make(map[string][]*flag.Flag)
	groupOrder := make([]string, 0)

	fs.VisitAll(func(f *flag.Flag) {
		group := flagDomain(f.Name)
		if _, exists := groups[group]; !exists {
			groupOrder = append(groupOrder, group)
		}

		groups[group] = append(groups[group], f)
	})

	sort.Slice(groupOrder, func(i, j int) bool {
		return domainTitle(groupOrder[i]) < domainTitle(groupOrder[j])
	})

	for _, group := range groupOrder {
		fmt.Fprintf(w, "%s:\n", domainTitle(group))

		for _, f := range groups[group] {
			printFlagWithDefault(w, f)
		}

		fmt.Fprintln(w)
	}
}

func printFlagWithDefault(w io.Writer, f *flag.Flag) {
	name := "  --" + f.Name

	usage := f.Usage + " (default: " + defaultValueForFlag(f) + ")"

	// Two-column format: flag name on the left, usage on the right.
	if len(name) < 30 {
		fmt.Fprintf(w, "%-30s %s\n", name, usage)
	} else {
		fmt.Fprintf(w, "%s\n%-30s %s\n", name, "", usage)
	}
}

func defaultValueForFlag(f *flag.Flag) string {
	if f == nil {
		return strconv.Quote("")
	}

	if value, ok := implicitFlagDefaults[f.Name]; ok {
		return value
	}

	if f.DefValue == "" {
		return strconv.Quote("")
	}

	return f.DefValue
}

func flagNamesForDomain(fs *flag.FlagSet, domainName string) []string {
	if fs == nil {
		return nil
	}

	var names []string
	fs.VisitAll(func(f *flag.Flag) {
		if flagBelongsToDomain(f.Name, domainName) {
			names = append(names, "--"+f.Name)
		}
	})
	sort.Strings(names)

	return names
}

func flagBelongsToDomain(flagName, domainName string) bool {
	if flagDomain(flagName) == domainName {
		return true
	}

	switch domainName {
	case codeIntelDomainName, configDomainName:
		return flagName == outputFlagName || flagName == commandOutputJSON
	default:
		return false
	}
}

func printWrappedList(w io.Writer, values []string, indent string, width int) {
	line := indent

	for i, value := range values {
		if i > 0 {
			value = ", " + value
		}

		if len(line)+len(value) > width && strings.TrimSpace(line) != "" {
			fmt.Fprintln(w, line)
			line = indent + strings.TrimPrefix(value, ", ")
			continue
		}

		line += value
	}

	if strings.TrimSpace(line) != "" {
		fmt.Fprintln(w, line)
	}
}

func domainTitle(name string) string {
	if domain, ok := lookupHelpDomain(name); ok {
		return domain.Title
	}

	return "Other"
}

func flagDomain(name string) string {
	if domain, ok := lookupFlagDomain(name); ok {
		return domain
	}

	return "chat/session"
}

//nolint:cyclop // A single explicit compatibility map keeps domain help auditable.
func lookupFlagDomain(name string) (string, bool) {
	name = normalizeHelpName(name)

	switch {
	case name == "once" || name == "stdin" || name == "output" || name == "autonomy" || name == "headless" ||
		name == "list-headless" || name == "stream-headless" ||
		name == "status-headless" || name == "cancel-headless" || name == "recover-headless" ||
		name == "headless-id" || name == "headless-private-log" ||
		name == helpSelectorSession || name == "session-id" || name == "session-dir" ||
		name == "session-title" || name == "session-tag" ||
		strings.HasPrefix(name, "list-session") || strings.HasPrefix(name, "show-session") ||
		strings.HasPrefix(name, "session-summary") || name == "replay" ||
		strings.HasPrefix(name, "export-session") || name == "export-format" ||
		name == "search-sessions" || name == "list-messages" || name == "list-artifacts" ||
		name == "list-failures" || name == "list-runs" || name == "show-run" ||
		name == "export-run" || name == "replay-run" || name == "resume-run" ||
		name == "record-failure" || name == "failure-reason" ||
		strings.HasPrefix(name, "failure-") || name == "record-artifact" || strings.HasPrefix(name, "artifact-") ||
		strings.HasPrefix(name, "merge-artifacts") || name == "merge-artifact-max-bytes":
		return "chat/session", true
	case name == configDomainName || name == "print-config-template" || name == "init-config" ||
		name == "list-config-paths" || name == "validate-config" || name == "config-migrate" || name == "config-report" ||
		name == "explain-config" || name == "explain-config-field" ||
		name == "doctor" || name == "doctor-offline" || name == "version" ||
		name == "state-diagnostics" || strings.HasPrefix(name, "command-surface-") ||
		strings.HasPrefix(name, "list-hook-events"):
		return configDomainName, true
	case name == "model" || name == "list-models" || name == "list-known-models" ||
		name == "list-providers" || name == "explain-model-resolution" ||
		name == commandOllamaStatus || name == commandOllamaStop ||
		name == "temperature" || name == "top-p" ||
		name == "max-tokens" || name == "seed" || name == "model-mode" || name == "reasoning-level" ||
		name == "max-input-tokens" || strings.HasPrefix(name, "route-"):
		return "providers", true
	case name == "agent" || name == "list-agents" || name == "describe-agent" ||
		name == "agent-performance-summary" ||
		strings.HasPrefix(name, "plan-") || strings.HasPrefix(name, "prompt-complete") ||
		name == "prompt-local-only" ||
		strings.HasPrefix(name, "async-") || strings.HasPrefix(name, "spawn-") ||
		strings.HasPrefix(name, "speculate-") || strings.HasPrefix(name, "task-") ||
		strings.HasPrefix(name, "skill-") || strings.HasPrefix(name, "feedback-") ||
		name == bashCommandName || name == "bash-dir" || name == "bash-timeout-seconds":
		return "agents", true
	case strings.HasPrefix(name, "issue-") ||
		name == "open-pr" || name == "base" || name == "run-tests" || name == "run-lint" ||
		name == "update-docs" || name == "update-changelog":
		return issueCommandName, true
	case name == "vectorizer" ||
		strings.HasPrefix(name, "memory-") || strings.HasPrefix(name, "agent-memory-") ||
		strings.HasPrefix(name, "retrieval-") ||
		strings.HasPrefix(name, "vector-") || strings.HasPrefix(name, "git-history-") ||
		strings.HasPrefix(name, "context-pack-"):
		return "memory/retrieval", true
	case name == commandOutputJSON || strings.HasPrefix(name, "code-") || strings.HasPrefix(name, "lsp-"):
		return codeIntelDomainName, true
	case name == "sentry" || strings.HasPrefix(name, "incident-"):
		return "incident", true
	case strings.HasPrefix(name, "review-"):
		return "review", true
	case strings.HasPrefix(name, "watch-"):
		return "watch", true
	case strings.HasPrefix(name, "plugin-") || strings.HasPrefix(name, "mcp-") ||
		name == "list-plugins" || name == "describe-plugin" || name == "run-plugin" ||
		name == "init-rtk-plugin":
		return "plugins", true
	case strings.HasPrefix(name, "worktree-") || name == "worktree" || name == "no-auto-merge" ||
		name == "list-worktrees" || name == "merge-worktree" ||
		name == "merge-worktree-allow-base-mismatch":
		return "worktrees", true
	case strings.HasPrefix(name, "eval-") || strings.HasPrefix(name, "evaluation-") ||
		name == "record-evaluation" || name == "list-evaluations" ||
		name == "record-response" || name == "replay-response":
		return "eval", true
	default:
		return "", false
	}
}
