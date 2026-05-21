package llm

import (
	"context"
	"fmt"
	"regexp"
	"strings"
)

// BashTool returns the standard tool definition that lets an LLM execute
// shell commands. The schema follows the OpenAI/Anthropic function-calling
// conventions with a single required "command" parameter.
func BashTool() ToolDefinition {
	return ToolDefinition{
		Name:        "bash",
		Description: "Execute a bash command and return stdout, stderr, and the exit status. Use this to run shell commands, inspect files, run tests, etc.",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"command": map[string]any{
					"type":        "string",
					"description": "The bash command to execute.",
				},
			},
			"required":             []string{"command"},
			"additionalProperties": false,
		},
	}
}

// DefaultTools returns the standard set of tools available to agents.
func DefaultTools() []ToolDefinition {
	return []ToolDefinition{BashTool()}
}

var (
	remoteScriptPattern         = regexp.MustCompile(`(?i)\b(curl|wget)\b[^\n;]*\|\s*(sh|bash|zsh)\b`)
	forkBombPattern             = regexp.MustCompile(`:\s*\(\s*\)\s*\{\s*:\s*\|\s*:\s*&\s*\}`)
	criticalRemovalPathPrefixes = []string{
		"/Applications",
		"/Library",
		"/System",
		"/Users",
		"/bin",
		"/etc",
		"/home",
		"/private",
		"/root",
		"/sbin",
		"/usr",
		"/var",
	}
)

const (
	dependencyActionAdd     = "add"
	dependencyActionInstall = "install"
)

// BashToolPolicy returns a conservative policy for the built-in bash tool. It
// allows ordinary read/build/test commands, denies obvious system-destructive
// commands, and requires confirmation for privileged or dependency-changing
// commands.
func BashToolPolicy(_ context.Context, call ToolCall, _ AgentLoopBudgetSnapshot) ToolPolicyDecision {
	if call.Name != "bash" {
		return ToolPolicyDecision{
			Verdict:     ToolPolicyDeny,
			Reason:      fmt.Sprintf("unknown tool %q", call.Name),
			MatchedRule: "bash.deny.unknown_tool",
		}
	}

	command, ok := call.Input["command"].(string)
	if !ok || strings.TrimSpace(command) == "" {
		return ToolPolicyDecision{
			Verdict:     ToolPolicyDeny,
			Reason:      "bash command is empty or not a string",
			MatchedRule: "bash.deny.empty_command",
		}
	}

	if strings.ContainsRune(command, '\x00') {
		return ToolPolicyDecision{
			Verdict:     ToolPolicyDeny,
			Reason:      "bash command contains a NUL byte",
			MatchedRule: "bash.deny.nul_byte",
		}
	}

	if reason, rule := deniedBashCommand(command); rule != "" {
		return ToolPolicyDecision{
			Verdict:     ToolPolicyDeny,
			Reason:      reason,
			MatchedRule: rule,
		}
	}

	if reason, rule := bashCommandRequiresConfirmation(command); rule != "" {
		return ToolPolicyDecision{
			Verdict:     ToolPolicyRequireConfirm,
			Reason:      reason,
			MatchedRule: rule,
		}
	}

	return ToolPolicyDecision{
		Verdict:     ToolPolicyAllow,
		Reason:      "bash command passed built-in policy checks",
		MatchedRule: "bash.allow.default",
	}
}

func deniedBashCommand(command string) (reason, rule string) {
	if forkBombPattern.MatchString(command) {
		return "fork-bomb pattern is not allowed", "bash.deny.fork_bomb"
	}

	for _, fields := range bashCommandSegments(command) {
		if len(fields) == 0 {
			continue
		}

		fields = unwrapBashCommandFields(fields)
		if len(fields) == 0 {
			continue
		}

		name := strings.ToLower(fields[0])
		switch name {
		case "shutdown", "reboot", "halt", "poweroff":
			return "system power command is not allowed", "bash.deny.power"
		case "mkfs", "fdisk", "parted":
			return "disk formatting/partitioning command is not allowed", "bash.deny.disk"
		case "diskutil":
			if len(fields) > 1 && strings.EqualFold(fields[1], "erase") {
				return "disk erase command is not allowed", "bash.deny.disk"
			}
		case "dd":
			if ddWritesDevice(fields[1:]) {
				return "writing directly to a device is not allowed", "bash.deny.device_write"
			}
		case "rm":
			if rmTargetsCriticalPath(fields[1:]) {
				return "recursive removal of critical paths is not allowed", "bash.deny.rm_critical"
			}
		}
	}

	return "", ""
}

func bashCommandRequiresConfirmation(command string) (reason, rule string) {
	if remoteScriptPattern.MatchString(command) {
		return "piping a remote download into a shell requires confirmation", "bash.confirm.remote_script"
	}

	for _, fields := range bashCommandSegments(command) {
		if len(fields) == 0 {
			continue
		}

		switch strings.ToLower(fields[0]) {
		case "sudo", "su":
			return "privileged command requires confirmation", "bash.confirm.privileged"
		}

		fields = unwrapBashCommandFields(fields)
		if len(fields) == 0 {
			continue
		}

		name := strings.ToLower(fields[0])
		switch name {
		case "git":
			if gitCommandRequiresConfirmation(fields[1:]) {
				return "destructive git command requires confirmation", "bash.confirm.git_destructive"
			}
		case "go", "npm", "pnpm", "yarn", "pip", "poetry", "cargo", "brew":
			if dependencyCommandRequiresConfirmation(name, fields[1:]) {
				return "dependency-changing command requires confirmation", "bash.confirm.dependency_change"
			}
		}
	}

	return "", ""
}

func bashCommandSegments(command string) [][]string {
	parts := strings.FieldsFunc(command, func(r rune) bool {
		switch r {
		case ';', '&', '|', '\n', '\r', '(', ')':
			return true
		default:
			return false
		}
	})

	segments := make([][]string, 0, len(parts))
	for _, part := range parts {
		fields := strings.Fields(strings.TrimSpace(part))
		if len(fields) > 0 {
			segments = append(segments, fields)
		}
	}

	return segments
}

func ddWritesDevice(args []string) bool {
	for _, arg := range args {
		arg = strings.ToLower(strings.TrimSpace(arg))
		if strings.HasPrefix(arg, "of=/dev/") {
			return true
		}
	}

	return false
}

func rmTargetsCriticalPath(args []string) bool {
	recursive := false

	for _, arg := range args {
		arg = strings.TrimSpace(arg)
		if arg == "" {
			continue
		}

		if strings.HasPrefix(arg, "-") {
			recursive = recursive || strings.Contains(arg, "r") || strings.Contains(arg, "R")
			continue
		}

		if recursive && isCriticalRemovalTarget(arg) {
			return true
		}
	}

	return false
}

func isCriticalRemovalTarget(target string) bool {
	target = normalizeRemovalTarget(target)
	if target == "" || target == "/" {
		return true
	}

	switch target {
	case ".", "..", "*", "./*", ".//*", "/*", ".git", "~", "$HOME", "${HOME}":
		return true
	}

	return strings.HasPrefix(target, "~/") ||
		strings.HasPrefix(target, "$HOME/") ||
		strings.HasPrefix(target, "${HOME}/") ||
		strings.HasPrefix(target, ".git/") ||
		hasAnyPathPrefix(target, criticalRemovalPathPrefixes)
}

func unwrapBashCommandFields(fields []string) []string {
	for len(fields) > 0 {
		switch strings.ToLower(fields[0]) {
		case "sudo":
			fields = fields[1:]
			for len(fields) > 0 && strings.HasPrefix(fields[0], "-") {
				fields = fields[1:]
			}
		case "command":
			fields = fields[1:]
		case "env":
			fields = fields[1:]
			for len(fields) > 0 {
				if fields[0] == "--" {
					fields = fields[1:]
					break
				}

				if strings.HasPrefix(fields[0], "-") || strings.Contains(fields[0], "=") {
					fields = fields[1:]
					continue
				}

				break
			}
		default:
			return fields
		}
	}

	return fields
}

func normalizeRemovalTarget(target string) string {
	target = strings.TrimSpace(target)
	for len(target) >= 2 {
		first, last := target[0], target[len(target)-1]
		if (first != '"' && first != '\'') || first != last {
			break
		}

		target = strings.TrimSpace(target[1 : len(target)-1])
	}

	return strings.TrimRight(target, "/")
}

func hasAnyPathPrefix(target string, prefixes []string) bool {
	for _, prefix := range prefixes {
		if hasPathPrefix(target, prefix) {
			return true
		}
	}

	return false
}

func hasPathPrefix(target, prefix string) bool {
	return target == prefix || strings.HasPrefix(target, prefix+"/")
}

func gitCommandRequiresConfirmation(args []string) bool {
	if len(args) == 0 {
		return false
	}

	switch strings.ToLower(args[0]) {
	case "reset":
		return containsArg(args[1:], "--hard")
	case "clean":
		return containsAnyFlag(args[1:], "f") && containsAnyFlag(args[1:], "d")
	default:
		return false
	}
}

func dependencyCommandRequiresConfirmation(name string, args []string) bool {
	if len(args) == 0 {
		return false
	}

	first := strings.ToLower(args[0])

	switch name {
	case "go":
		return first == "get" || first == dependencyActionInstall
	case "npm", "pnpm":
		return first == dependencyActionInstall || first == dependencyActionAdd
	case "yarn":
		return first == dependencyActionAdd
	case "pip":
		return first == dependencyActionInstall
	case "poetry":
		return first == dependencyActionAdd
	case "cargo", "brew":
		return first == dependencyActionInstall
	default:
		return false
	}
}

func containsArg(args []string, want string) bool {
	for _, arg := range args {
		if strings.EqualFold(arg, want) {
			return true
		}
	}

	return false
}

func containsAnyFlag(args []string, flag string) bool {
	for _, arg := range args {
		if strings.HasPrefix(arg, "-") && strings.Contains(arg, flag) {
			return true
		}
	}

	return false
}
