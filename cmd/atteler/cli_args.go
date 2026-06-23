package main

import (
	"errors"
	"flag"
	"fmt"
	"strings"
)

const issueWatchCommandFlagName = "command"

func translateCLIArgs(args []string) cliArgPlan {
	return translateCLIArgsWithFlagSet(args, flag.CommandLine)
}

func translateCLIArgsWithFlagSet(args []string, fs *flag.FlagSet) cliArgPlan {
	if len(args) == 0 {
		return cliArgPlan{Args: args}
	}

	if isHelpFlag(args[0]) {
		return cliArgPlan{Help: true}
	}

	prefix, rest := splitLeadingFlagArgs(args, fs)
	if len(rest) == 0 {
		return cliArgPlan{Args: append([]string(nil), args...)}
	}

	if isHelpFlag(rest[0]) {
		return cliArgPlan{Help: true}
	}

	if rest[0] == helpCommandName {
		return translateHelpCommandArgs(prefix, rest, fs)
	}

	domain, ok := lookupHelpDomain(rest[0])
	if !ok {
		return cliArgPlan{Args: append([]string(nil), args...)}
	}

	return translateDomainCommandArgs(prefix, domain, rest, fs)
}

func translateHelpCommandArgs(prefix, rest []string, fs *flag.FlagSet) cliArgPlan {
	if len(rest) == 1 || isHelpFlag(rest[1]) {
		return cliArgPlan{Help: true}
	}

	if len(rest) == 2 && normalizeHelpName(rest[1]) == helpCommandName {
		return cliArgPlan{Help: true}
	}

	if helpDomain, ok := helpDomainForSelector(rest[1], fs); ok {
		return cliArgPlan{Help: true, HelpDomain: helpDomain}
	}

	// Preserve the common positional prompt `atteler help me` without turning
	// every unknown `atteler help <selector>` typo into an LLM call.
	if len(rest) == 2 && normalizeHelpName(rest[1]) == "me" {
		return cliArgPlan{Args: appendCLIArgs(prefix, rest...)}
	}

	if len(rest) == 2 {
		return cliArgPlan{Help: true, HelpDomain: rest[1]}
	}

	// Preserve positional prompt compatibility for prompts like
	// `atteler help me write tests`; users can still request scoped help with
	// any known selector, e.g. `atteler help code-intel`.
	return cliArgPlan{Args: appendCLIArgs(prefix, rest...)}
}

func translateDomainCommandArgs(prefix []string, domain cliHelpDomain, rest []string, fs *flag.FlagSet) cliArgPlan {
	if len(rest) == 1 || isHelpArg(rest[1]) {
		return cliArgPlan{Help: true, HelpDomain: rest[0]}
	}

	// Let users scope old flag aliases under a domain while scripts continue to
	// use top-level flags unchanged: `atteler code-intel --code-summary`.
	// If a real grouped command follows domain-level flags, route the command
	// while keeping those flags as parseable global options:
	// `atteler session --session abc messages`.
	if strings.HasPrefix(rest[1], "-") {
		return translateDomainFlagPrefixArgs(prefix, domain, rest, fs)
	}

	usageName := usageNameForDomainSelector(domain, rest[0])

	command, ok := lookupDomainCommand(domain, rest[1])
	if !ok {
		if domain.Name == autoresearchDomainName {
			return translateDomainCommandArgs(prefix, domain, append([]string{rest[0], runCommandName}, rest[1:]...), fs)
		}

		// Domain words such as "review", "watch", "session", and "code" are
		// also natural prompt starters.  Only claim grouped routing when the
		// command token is known; otherwise keep the legacy positional prompt
		// path intact.
		if strictUnknownDomainCommand(domain, rest[0]) {
			return cliArgPlan{
				Err: unknownDomainCommandError(domain, rest[0], rest[1]),
			}
		}

		return cliArgPlan{Args: appendCLIArgs(prefix, rest...)}
	}

	if commandTailRequestsHelp(command, rest[2:]) {
		return cliArgPlan{Help: true, HelpDomain: rest[0]}
	}

	if domain.Name == issueCommandName {
		switch command.Name {
		case "watch":
			return translateIssueWatchCommandArgs(prefix, rest[2:], fs, false)
		case "list-candidates":
			return translateIssueWatchCommandArgs(prefix, rest[2:], fs, true)
		case runCommandName:
			return translateIssueRunCommandArgs(prefix, rest[2:], fs)
		}
	}

	if strictUnknownDomainCommand(domain, rest[0]) {
		if unknownFlag, ok := firstUnknownFlagArg(rest[2:], fs, false); ok {
			return cliArgPlan{Err: unknownDomainFlagError(domain, rest[0], unknownFlag)}
		}
	}

	if commandMissingRequiredArgs(command, prefix, rest[2:], fs) {
		return cliArgPlan{
			Err: fmt.Errorf("%s %s requires %s; run `atteler help %s`", usageName, command.Name, command.Args, usageName),
		}
	}

	return cliArgPlan{Args: appendCLIArgs(prefix, commandArgsWithFlagSet(command, prefix, rest[2:], fs)...)}
}

//nolint:cyclop,gocognit,wsl_v5 // Command-specific flag translation keeps --once from colliding with chat once.
func translateIssueWatchCommandArgs(prefix, tail []string, fs *flag.FlagSet, forceListCandidates bool) cliArgPlan {
	out := []string{"--issue-watch"}

	for i := 0; i < len(tail); i++ {
		arg := tail[i]
		if arg == "--" {
			if i+1 < len(tail) {
				return cliArgPlan{Err: fmt.Errorf("issue watch does not accept positional arguments: %q", tail[i+1])}
			}

			break
		}

		name, value, hasValue := splitIssueWatchFlag(arg)
		switch name {
		case "github":
			next, nextIndex, err := issueWatchFlagValue(name, value, hasValue, tail, i)
			if err != nil {
				return cliArgPlan{Err: err}
			}
			out = append(out, "--issue-watch-github", next)
			i = nextIndex
		case "label":
			next, nextIndex, err := issueWatchFlagValue(name, value, hasValue, tail, i)
			if err != nil {
				return cliArgPlan{Err: err}
			}
			out = append(out, "--issue-watch-label", next)
			i = nextIndex
		case "github-api-url":
			next, nextIndex, err := issueWatchFlagValue(name, value, hasValue, tail, i)
			if err != nil {
				return cliArgPlan{Err: err}
			}
			out = append(out, "--issue-watch-github-api-url", next)
			i = nextIndex
		case "github-token":
			next, nextIndex, err := issueWatchFlagValue(name, value, hasValue, tail, i)
			if err != nil {
				return cliArgPlan{Err: err}
			}
			out = append(out, "--issue-watch-github-token", next)
			i = nextIndex
		case "interval-seconds":
			next, nextIndex, err := issueWatchFlagValue(name, value, hasValue, tail, i)
			if err != nil {
				return cliArgPlan{Err: err}
			}
			out = append(out, "--issue-watch-interval-seconds", next)
			i = nextIndex
		case issueWatchCommandFlagName:
			next, nextIndex, err := issueWatchFlagValue(name, value, hasValue, tail, i)
			if err != nil {
				return cliArgPlan{Err: err}
			}
			out = append(out, "--issue-watch-command", next)
			i = nextIndex
		case "validation-command":
			next, nextIndex, err := issueWatchFlagValue(name, value, hasValue, tail, i)
			if err != nil {
				return cliArgPlan{Err: err}
			}
			out = append(out, "--issue-watch-validation-command", next)
			i = nextIndex
		case "command-timeout-seconds":
			next, nextIndex, err := issueWatchFlagValue(name, value, hasValue, tail, i)
			if err != nil {
				return cliArgPlan{Err: err}
			}
			out = append(out, "--issue-watch-command-timeout-seconds", next)
			i = nextIndex
		case onceCommandName:
			out = appendIssueWatchBoolFlag(out, "--issue-watch-once", value, hasValue)
		case issueWatchDryRunModeName:
			out = appendIssueWatchBoolFlag(out, "--issue-watch-dry-run", value, hasValue)
		default:
			if strings.HasPrefix(arg, "-") {
				var ok bool
				out, i, ok = appendKnownIssueWatchFlag(out, tail, i, fs)
				if ok {
					continue
				}

				return cliArgPlan{Err: fmt.Errorf("unknown issue watch flag %q; run `atteler help issue`", arg)}
			}

			return cliArgPlan{Err: fmt.Errorf("issue watch does not accept positional arguments: %q", arg)}
		}
	}

	if forceListCandidates {
		out = append(out, "--issue-watch-dry-run", "--issue-watch-once")
	}

	return cliArgPlan{Args: appendCLIArgs(prefix, out...)}
}

//nolint:cyclop,gocognit,wsl_v5 // Command-specific flag translation keeps GitHub issue refs and watch flags unambiguous.
func translateIssueRunCommandArgs(prefix, tail []string, fs *flag.FlagSet) cliArgPlan {
	out := []string{"--issue-watch", "--issue-watch-once"}
	runRef := ""

	for i := 0; i < len(tail); i++ {
		arg := tail[i]
		if arg == "--" {
			if i+1 >= len(tail) {
				break
			}
			if runRef != "" {
				return cliArgPlan{Err: fmt.Errorf("issue run accepts exactly one issue reference; got extra argument %q", tail[i+1])}
			}

			runRef = tail[i+1]
			if i+2 < len(tail) {
				return cliArgPlan{Err: fmt.Errorf("issue run accepts exactly one issue reference; got extra argument %q", tail[i+2])}
			}

			break
		}

		name, value, hasValue := splitIssueWatchFlag(arg)
		switch name {
		case "github":
			next, nextIndex, err := issueWatchFlagValue(name, value, hasValue, tail, i)
			if err != nil {
				return cliArgPlan{Err: err}
			}
			out = append(out, "--issue-watch-github", next)
			i = nextIndex
		case "label":
			next, nextIndex, err := issueWatchFlagValue(name, value, hasValue, tail, i)
			if err != nil {
				return cliArgPlan{Err: err}
			}
			out = append(out, "--issue-watch-label", next)
			i = nextIndex
		case "github-api-url":
			next, nextIndex, err := issueWatchFlagValue(name, value, hasValue, tail, i)
			if err != nil {
				return cliArgPlan{Err: err}
			}
			out = append(out, "--issue-watch-github-api-url", next)
			i = nextIndex
		case "github-token":
			next, nextIndex, err := issueWatchFlagValue(name, value, hasValue, tail, i)
			if err != nil {
				return cliArgPlan{Err: err}
			}
			out = append(out, "--issue-watch-github-token", next)
			i = nextIndex
		case issueWatchCommandFlagName:
			next, nextIndex, err := issueWatchFlagValue(name, value, hasValue, tail, i)
			if err != nil {
				return cliArgPlan{Err: err}
			}
			out = append(out, "--issue-watch-command", next)
			i = nextIndex
		case "validation-command":
			next, nextIndex, err := issueWatchFlagValue(name, value, hasValue, tail, i)
			if err != nil {
				return cliArgPlan{Err: err}
			}
			out = append(out, "--issue-watch-validation-command", next)
			i = nextIndex
		case "command-timeout-seconds":
			next, nextIndex, err := issueWatchFlagValue(name, value, hasValue, tail, i)
			if err != nil {
				return cliArgPlan{Err: err}
			}
			out = append(out, "--issue-watch-command-timeout-seconds", next)
			i = nextIndex
		case onceCommandName:
			// issue run is intrinsically one-shot; accept --once without routing it
			// to the chat one-shot flag, which would consume the issue reference.
		case issueWatchDryRunModeName:
			out = appendIssueWatchBoolFlag(out, "--issue-watch-dry-run", value, hasValue)
		default:
			if strings.HasPrefix(arg, "-") {
				var ok bool
				out, i, ok = appendKnownIssueWatchFlag(out, tail, i, fs)
				if ok {
					continue
				}

				return cliArgPlan{Err: fmt.Errorf("unknown issue run flag %q; run `atteler help issue`", arg)}
			}
			if runRef != "" {
				return cliArgPlan{Err: fmt.Errorf("issue run accepts exactly one issue reference; got extra argument %q", arg)}
			}

			runRef = arg
		}
	}

	if strings.TrimSpace(runRef) == "" {
		return cliArgPlan{Err: errors.New("issue run requires an issue reference; run `atteler help issue`")}
	}

	out = append(out, "--issue-watch-run", runRef)

	return cliArgPlan{Args: appendCLIArgs(prefix, out...)}
}

func appendKnownIssueWatchFlag(args, tail []string, index int, fs *flag.FlagSet) (out []string, nextIndex int, ok bool) {
	arg := tail[index]
	if !knownFlagArg(arg, fs) || !issueWatchPassthroughFlag(arg) {
		return args, index, false
	}

	args = append(args, arg)
	if flagConsumesSeparateValue(arg, fs) && index+1 < len(tail) && !isFlagArg(tail[index+1]) {
		args = append(args, tail[index+1])
		index++
	}

	return args, index, true
}

func issueWatchPassthroughFlag(arg string) bool {
	name := normalizeHelpName(flagName(arg))
	switch name {
	case "autonomy",
		"permission-mode",
		"allow-operation",
		"deny-operation",
		"issue-watch-github",
		"issue-watch-label",
		"issue-watch-once",
		"issue-watch-dry-run",
		"issue-watch-github-api-url",
		"issue-watch-github-token",
		"issue-watch-run",
		"issue-watch-interval-seconds",
		"issue-watch-command",
		"issue-watch-validation-command",
		"issue-watch-command-timeout-seconds":
		return true
	default:
		return false
	}
}

func splitIssueWatchFlag(arg string) (name, value string, hasValue bool) {
	name = strings.TrimLeft(arg, "-")
	if before, after, ok := strings.Cut(name, "="); ok {
		return before, after, true
	}

	return name, "", false
}

func issueWatchFlagValue(name, inlineValue string, hasValue bool, tail []string, index int) (value string, nextIndex int, err error) {
	if hasValue {
		if strings.TrimSpace(inlineValue) == "" {
			return "", index, fmt.Errorf("issue watch flag --%s requires a value", name)
		}

		return inlineValue, index, nil
	}

	if index+1 >= len(tail) || strings.HasPrefix(tail[index+1], "-") {
		return "", index, fmt.Errorf("issue watch flag --%s requires a value", name)
	}

	return tail[index+1], index + 1, nil
}

func appendIssueWatchBoolFlag(args []string, flagName, value string, hasValue bool) []string {
	if !hasValue {
		return append(args, flagName)
	}

	return append(args, flagName+"="+value)
}

func strictUnknownDomainCommand(domain cliHelpDomain, selector string) bool {
	if domain.Name != codeIntelDomainName && domain.Name != issueCommandName {
		return false
	}

	selector = normalizeHelpName(selector)
	switch selector {
	case codeIntelDomainName, "codeintel", "code-intelligence":
		return true
	case issueCommandName, "issues":
		return true
	default:
		return false
	}
}

func unknownDomainCommandError(domain cliHelpDomain, selector, command string) error {
	usageName := usageNameForDomainSelector(domain, selector)

	return fmt.Errorf("unknown %s command %q; run `atteler help %s`", usageName, command, usageName)
}

func unknownDomainFlagError(domain cliHelpDomain, selector, flagArg string) error {
	usageName := usageNameForDomainSelector(domain, selector)

	return fmt.Errorf("unknown %s flag %q; run `atteler help %s`", usageName, flagArg, usageName)
}

func translateDomainFlagPrefixArgs(prefix []string, domain cliHelpDomain, rest []string, fs *flag.FlagSet) cliArgPlan {
	if !knownFlagArg(rest[1], fs) {
		if strictUnknownDomainCommand(domain, rest[0]) {
			return cliArgPlan{Err: unknownDomainFlagError(domain, rest[0], rest[1])}
		}

		return cliArgPlan{Args: appendCLIArgs(prefix, rest...)}
	}

	if strictUnknownDomainCommand(domain, rest[0]) {
		if unknownFlag, ok := firstUnknownFlagArg(rest[1:], fs, true); ok {
			return cliArgPlan{Err: unknownDomainFlagError(domain, rest[0], unknownFlag)}
		}
	}

	if tailRequestsHelpFlag(rest[1:]) {
		return cliArgPlan{Help: true, HelpDomain: rest[0]}
	}

	scopedPrefix, commandRest := splitLeadingFlagArgs(rest[1:], fs)
	if len(commandRest) == 0 {
		if domain.Name == autoresearchDomainName {
			return cliArgPlan{Args: appendCLIArgs(appendCLIArgs(prefix, scopedPrefix...), "--autoresearch")}
		}

		if strictUnknownDomainCommand(domain, rest[0]) && !codeIntelFlagPrefixSelectsCommand(scopedPrefix, fs) {
			return cliArgPlan{Err: missingDomainCommandError(domain, rest[0])}
		}

		return cliArgPlan{Args: appendCLIArgs(prefix, rest[1:]...)}
	}

	if _, ok := lookupDomainCommand(domain, commandRest[0]); !ok {
		if domain.Name == autoresearchDomainName {
			return translateDomainCommandArgs(appendCLIArgs(prefix, scopedPrefix...), domain, append([]string{rest[0], runCommandName}, commandRest...), fs)
		}

		if strictUnknownDomainCommand(domain, rest[0]) {
			return cliArgPlan{Err: unknownDomainCommandError(domain, rest[0], commandRest[0])}
		}

		return cliArgPlan{Args: appendCLIArgs(prefix, rest...)}
	}

	return translateDomainCommandArgs(appendCLIArgs(prefix, scopedPrefix...), domain, append([]string{rest[0]}, commandRest...), fs)
}

func missingDomainCommandError(domain cliHelpDomain, selector string) error {
	usageName := usageNameForDomainSelector(domain, selector)

	return fmt.Errorf("%s requires a command; run `atteler help %s`", usageName, usageName)
}

func firstUnknownFlagArg(args []string, fs *flag.FlagSet, stopAtPositional bool) (string, bool) {
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--" || isHelpFlag(arg) {
			return "", false
		}

		if !isFlagArg(arg) {
			if stopAtPositional {
				return "", false
			}

			continue
		}

		if !knownFlagArg(arg, fs) {
			return arg, true
		}

		if flagConsumesSeparateValue(arg, fs) && i+1 < len(args) {
			i++
		}
	}

	return "", false
}

func codeIntelFlagPrefixSelectsCommand(args []string, fs *flag.FlagSet) bool {
	selectors := codeIntelSelectorFlagSet()

	for i := 0; i < len(args); i++ {
		arg := args[i]
		name := flagName(arg)

		if !selectors["--"+name] {
			i = skipFlagValueIndex(args, i, fs)

			continue
		}

		selected, nextIndex := codeIntelSelectorArgSelectsCommand(args, i, fs)
		if selected {
			return true
		}

		i = nextIndex
	}

	return false
}

func skipFlagValueIndex(args []string, index int, fs *flag.FlagSet) int {
	if flagConsumesSeparateValue(args[index], fs) && index+1 < len(args) {
		return index + 1
	}

	return index
}

func codeIntelSelectorArgSelectsCommand(args []string, index int, fs *flag.FlagSet) (selected bool, nextIndex int) {
	arg := args[index]
	name := flagName(arg)

	consumesValue := flagConsumesSeparateValue(arg, fs)
	if !consumesValue {
		return flagArgsEnableBool([]string{arg}, name), index
	}

	if _, value, hasValue := strings.Cut(arg, "="); hasValue {
		return strings.TrimSpace(value) != "", index
	}

	if index+1 >= len(args) {
		return true, index
	}

	if strings.TrimSpace(args[index+1]) == "" {
		return false, index + 1
	}

	return true, index
}

func codeIntelSelectorFlagSet() map[string]bool {
	selectors := make(map[string]bool, len(codeIntelCommandDescriptors())+2)
	for _, descriptor := range codeIntelCommandDescriptors() {
		selectors[descriptor.LegacyFlag] = true
	}

	selectors["--lsp-symbols"] = true
	selectors["--lsp-workspace-symbols"] = true

	return selectors
}

func commandTailRequestsHelp(command cliCommandAlias, tail []string) bool {
	if command.OpaqueArgs {
		return false
	}

	return tailRequestsHelpFlag(tail)
}

func tailRequestsHelpFlag(tail []string) bool {
	for _, arg := range tail {
		if arg == "--" {
			return false
		}

		if isHelpFlag(arg) {
			return true
		}
	}

	return false
}

func helpDomainForSelector(name string, fs *flag.FlagSet) (string, bool) {
	if strings.HasPrefix(strings.TrimSpace(name), "-") {
		return helpDomainForFlagSelector(name, fs)
	}

	switch normalizeHelpName(name) {
	case "legacy", "flags", helpSelectorAll, "domains", "overview":
		return normalizeHelpName(name), true
	}

	if _, ok := lookupHelpDomain(name); ok {
		return strings.TrimSpace(name), true
	}

	return helpDomainForFlagSelector(name, fs)
}

func helpDomainForFlagSelector(name string, fs *flag.FlagSet) (string, bool) {
	name = normalizeHelpName(flagName(name))
	if name == "" {
		return "", false
	}

	if fs == nil || fs.Lookup(name) == nil {
		return "", false
	}

	return lookupFlagDomain(name)
}

func commandMissingRequiredArgs(command cliCommandAlias, prefix, tail []string, fs *flag.FlagSet) bool {
	if !strings.Contains(command.Args, "<") {
		return false
	}

	if command.OpaqueArgs {
		return len(tail) == 0
	}

	prefixFlagArgs, _, _ := splitFlagArgs(prefix, fs)
	flagArgs, positionalArgs, _ := splitFlagArgs(tail, fs)
	allFlagArgs := combinedFlagArgs(prefixFlagArgs, flagArgs)

	if command.PromptAfterValue {
		if len(positionalArgs) == 0 {
			return true
		}

		return len(positionalArgs) < 2 && !flagArgsEnableBool(allFlagArgs, "stdin")
	}

	if command.PromptFromStdin {
		return len(positionalArgs) == 0 && !flagArgsEnableBool(allFlagArgs, "stdin")
	}

	return len(positionalArgs) == 0
}

func isHelpArg(arg string) bool {
	return arg == helpCommandName || isHelpFlag(arg)
}

func isHelpFlag(arg string) bool {
	return arg == helpLongFlag || arg == helpGoFlag || arg == helpShortFlag
}

func commandArgsWithFlagSet(command cliCommandAlias, prefix, tail []string, fs *flag.FlagSet) []string {
	if command.OpaqueArgs {
		return opaqueCommandArgs(command, tail)
	}

	if command.PromptAfterValue {
		return commandArgsWithPromptAfterValue(command, tail, fs)
	}

	prefixFlagArgs, _, _ := splitFlagArgs(prefix, fs)
	flagArgs, positionalArgs, hadDelimiter := splitFlagArgs(tail, fs)

	if command.PromptFromStdin && len(positionalArgs) == 0 && flagArgsEnableBool(combinedFlagArgs(prefixFlagArgs, flagArgs), "stdin") {
		return flagArgs
	}

	out := make([]string, 0, len(flagArgs)+len(command.Legacy)+len(positionalArgs))
	out = append(out, command.Legacy...)

	if command.JoinArgs && len(positionalArgs) > 0 {
		joined := strings.Join(positionalArgs, " ")
		if legacyConsumesSeparateValue(command, fs) {
			out = append(out, joined)
			out = append(out, flagArgs...)

			return out
		}

		out = append(out, flagArgs...)
		out = appendDelimiterForDashPositional(out, hadDelimiter, joined)
		out = append(out, joined)

		return out
	}

	if legacyConsumesSeparateValue(command, fs) && len(positionalArgs) > 0 {
		out = append(out, positionalArgs[0])
		out = append(out, flagArgs...)
		out = append(out, positionalArgs[1:]...)

		return out
	}

	out = append(out, flagArgs...)
	if len(positionalArgs) > 0 {
		out = appendDelimiterForDashPositional(out, hadDelimiter, positionalArgs[0])
	}

	out = append(out, positionalArgs...)

	return out
}

func combinedFlagArgs(first, second []string) []string {
	out := make([]string, 0, len(first)+len(second))
	out = append(out, first...)
	out = append(out, second...)

	return out
}

func commandArgsWithPromptAfterValue(command cliCommandAlias, tail []string, fs *flag.FlagSet) []string {
	flagArgs, positionalArgs, _ := splitFlagArgs(tail, fs)
	out := make([]string, 0, len(command.Legacy)+len(flagArgs)+3)
	out = append(out, command.Legacy...)

	if len(positionalArgs) > 0 {
		out = append(out, positionalArgs[0])
	}

	if len(positionalArgs) > 1 {
		out = append(out, "--once", strings.Join(positionalArgs[1:], " "))
	}

	out = append(out, flagArgs...)

	return out
}

func appendDelimiterForDashPositional(args []string, hadDelimiter bool, firstPositional string) []string {
	if hadDelimiter && isFlagArg(firstPositional) {
		return append(args, "--")
	}

	return args
}

func opaqueCommandArgs(command cliCommandAlias, tail []string) []string {
	out := make([]string, 0, len(command.Legacy)+1)
	out = append(out, command.Legacy...)

	if len(tail) > 0 {
		out = append(out, strings.Join(tail, " "))
	}

	return out
}

func legacyConsumesSeparateValue(command cliCommandAlias, fs *flag.FlagSet) bool {
	if len(command.Legacy) == 0 {
		return false
	}

	return flagConsumesSeparateValue(command.Legacy[len(command.Legacy)-1], fs)
}

func splitLeadingFlagArgs(args []string, fs *flag.FlagSet) (prefix, rest []string) {
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--" {
			return append([]string(nil), args[:i]...), args[i:]
		}

		if isHelpFlag(arg) {
			return append([]string(nil), args[:i]...), args[i:]
		}

		if !isFlagArg(arg) {
			return append([]string(nil), args[:i]...), args[i:]
		}

		if flagConsumesSeparateValue(arg, fs) && i+1 < len(args) {
			i++
		}
	}

	return append([]string(nil), args...), nil
}

func splitFlagArgs(args []string, fs *flag.FlagSet) (flagArgs, positionalArgs []string, hadDelimiter bool) {
	flagArgs = make([]string, 0, len(args))
	positionalArgs = make([]string, 0, len(args))

	positionalOnly := false

	for i := 0; i < len(args); i++ {
		arg := args[i]
		if positionalOnly {
			positionalArgs = append(positionalArgs, arg)
			continue
		}

		if arg == "--" {
			positionalOnly = true
			hadDelimiter = true

			continue
		}

		if !isFlagArg(arg) {
			positionalArgs = append(positionalArgs, arg)
			continue
		}

		flagArgs = append(flagArgs, arg)
		if flagConsumesSeparateValue(arg, fs) && i+1 < len(args) {
			i++
			flagArgs = append(flagArgs, args[i])
		}
	}

	return flagArgs, positionalArgs, hadDelimiter
}

func isFlagArg(arg string) bool {
	return strings.HasPrefix(arg, "-") && arg != "-"
}

func knownFlagArg(arg string, fs *flag.FlagSet) bool {
	name := flagName(arg)
	if name == "" {
		return false
	}

	if fs != nil && fs.Lookup(name) != nil {
		return true
	}

	_, ok := lookupFlagDomain(name)

	return ok
}

func flagConsumesSeparateValue(arg string, fs *flag.FlagSet) bool {
	name := flagName(arg)
	if name == "" || strings.Contains(arg, "=") {
		return false
	}

	if fs == nil {
		return true
	}

	f := fs.Lookup(name)
	if f == nil {
		return true
	}

	boolFlag, ok := f.Value.(interface{ IsBoolFlag() bool })

	return !ok || !boolFlag.IsBoolFlag()
}

func flagName(arg string) string {
	name := strings.TrimLeft(arg, "-")
	if before, _, ok := strings.Cut(name, "="); ok {
		name = before
	}

	return name
}

func flagArgsEnableBool(args []string, name string) bool {
	enabled := false

	for _, arg := range args {
		if flagName(arg) != name {
			continue
		}

		_, rawValue, hasValue := strings.Cut(arg, "=")
		if !hasValue {
			enabled = true
			continue
		}

		switch strings.ToLower(strings.TrimSpace(rawValue)) {
		case "", "0", "false", "no", "off":
			enabled = false
		default:
			enabled = true
		}
	}

	return enabled
}

func appendCLIArgs(prefix []string, args ...string) []string {
	out := make([]string, 0, len(prefix)+len(args))
	out = append(out, prefix...)
	out = append(out, args...)

	return out
}

func lookupDomainCommand(domain cliHelpDomain, name string) (cliCommandAlias, bool) {
	name = normalizeHelpName(name)

	if command, ok := lookupDomainCommandAlias(domain.Commands, name); ok {
		return command, true
	}

	return lookupDomainCommandAlias(domain.RoutingCommands, name)
}

func lookupDomainCommandAlias(commands []cliCommandAlias, name string) (cliCommandAlias, bool) {
	for i := range commands {
		command := &commands[i]
		if normalizeHelpName(command.Name) == name {
			return *command, true
		}

		for _, alias := range command.Aliases {
			if normalizeHelpName(alias) == name {
				return *command, true
			}
		}
	}

	return cliCommandAlias{}, false
}

func lookupHelpDomain(name string) (cliHelpDomain, bool) {
	name = normalizeHelpName(name)

	for i := range cliHelpDomains {
		domain := &cliHelpDomains[i]
		if normalizeHelpName(domain.Name) == name {
			return *domain, true
		}

		for _, alias := range domain.Aliases {
			if normalizeHelpName(alias) == name {
				return *domain, true
			}
		}

		for _, alias := range domain.HiddenAliases {
			if normalizeHelpName(alias) == name {
				return *domain, true
			}
		}
	}

	return cliHelpDomain{}, false
}

func normalizeHelpName(name string) string {
	name = strings.ToLower(strings.TrimSpace(name))
	name = strings.TrimPrefix(name, "--")
	name = strings.TrimPrefix(name, "-")
	name = strings.ReplaceAll(name, "_", "-")
	name = strings.ReplaceAll(name, "/", "-")

	return name
}
