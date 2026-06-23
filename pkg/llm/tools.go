package llm

import (
	"context"
	"fmt"
	"regexp"
	"slices"
	"strings"

	"github.com/tommoulard/atteler/pkg/autonomy"
	"github.com/tommoulard/atteler/pkg/permission"
)

// BashTool returns the standard tool definition that lets an LLM execute
// shell commands. The schema follows the OpenAI/Anthropic function-calling
// conventions with a single required "command" parameter.
func BashTool() ToolDefinition {
	return ToolDefinition{
		Name:        ToolNameBash,
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

// ReadTool returns the standard tool definition for reading a workspace file
// without shelling out.
func ReadTool() ToolDefinition {
	return ToolDefinition{
		Name:        ToolNameRead,
		Description: "Read a UTF-8 text file from the workspace without running a shell command. Use offset and limit for large files.",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"path": map[string]any{
					"type":        "string",
					"description": "File path to read, relative to the current working directory unless absolute.",
				},
				"offset": map[string]any{
					"type":        "integer",
					"minimum":     1,
					"description": "Optional 1-based line number to start reading from.",
				},
				"limit": map[string]any{
					"type":        "integer",
					"minimum":     1,
					"description": "Optional maximum number of lines to return.",
				},
			},
			"required":             []string{"path"},
			"additionalProperties": false,
		},
	}
}

// WriteTool returns the standard tool definition for overwriting a workspace
// file without shelling out.
func WriteTool() ToolDefinition {
	return ToolDefinition{
		Name:        ToolNameWrite,
		Description: "Create or overwrite a UTF-8 text file in the workspace without running a shell command.",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"path": map[string]any{
					"type":        "string",
					"description": "File path to create or overwrite, relative to the current working directory unless absolute.",
				},
				"content": map[string]any{
					"type":        "string",
					"description": "Full file contents to write.",
				},
				"create_parent_dirs": map[string]any{
					"type":        "boolean",
					"description": "Create missing parent directories before writing. Defaults to false.",
				},
			},
			"required":             []string{"path", "content"},
			"additionalProperties": false,
		},
	}
}

// EditTool returns the standard tool definition for replacing text in a
// workspace file without shelling out.
func EditTool() ToolDefinition {
	return ToolDefinition{
		Name:        ToolNameEdit,
		Description: "Replace text in a UTF-8 workspace file without running a shell command. By default old_string must match exactly once.",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"path": map[string]any{
					"type":        "string",
					"description": "File path to edit, relative to the current working directory unless absolute.",
				},
				"old_string": map[string]any{
					"type":        "string",
					"description": "Exact text to replace.",
				},
				"new_string": map[string]any{
					"type":        "string",
					"description": "Replacement text.",
				},
				"replace_all": map[string]any{
					"type":        "boolean",
					"description": "Replace every occurrence instead of requiring exactly one match. Defaults to false.",
				},
			},
			"required":             []string{"path", "old_string", "new_string"},
			"additionalProperties": false,
		},
	}
}

// GlobTool returns the standard tool definition for listing workspace files
// matching a glob pattern without shelling out.
func GlobTool() ToolDefinition {
	return ToolDefinition{
		Name:        ToolNameGlob,
		Description: "List workspace files matching a glob pattern without running a shell command. Supports ** path segments for recursive matches.",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"pattern": map[string]any{
					"type":        "string",
					"description": "Glob pattern to match, relative to path. Example: **/*.go",
				},
				"path": map[string]any{
					"type":        "string",
					"description": "Directory to search from. Defaults to the current working directory.",
				},
				"max_results": map[string]any{
					"type":        "integer",
					"minimum":     1,
					"maximum":     FileToolMaxResults,
					"description": fmt.Sprintf("Maximum number of matches to return. Defaults to 200 and cannot exceed %d.", FileToolMaxResults),
				},
			},
			"required":             []string{"pattern"},
			"additionalProperties": false,
		},
	}
}

// GrepTool returns the standard tool definition for searching workspace files
// with a regular expression without shelling out.
func GrepTool() ToolDefinition {
	return ToolDefinition{
		Name:        ToolNameGrep,
		Description: "Search UTF-8 workspace files with a regular expression without running a shell command.",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"pattern": map[string]any{
					"type":        "string",
					"description": "Go regular expression to search for.",
				},
				"path": map[string]any{
					"type":        "string",
					"description": "File or directory to search. Defaults to the current working directory.",
				},
				"include": map[string]any{
					"type":        "string",
					"description": "Optional glob for files to include relative to the search directory, such as **/*.go.",
				},
				"case_sensitive": map[string]any{
					"type":        "boolean",
					"description": "Whether matching is case-sensitive. Defaults to true.",
				},
				"max_results": map[string]any{
					"type":        "integer",
					"minimum":     1,
					"maximum":     FileToolMaxResults,
					"description": fmt.Sprintf("Maximum number of matching lines to return. Defaults to 100 and cannot exceed %d.", FileToolMaxResults),
				},
			},
			"required":             []string{"pattern"},
			"additionalProperties": false,
		},
	}
}

// DefaultTools returns the standard set of tools available to agents.
func DefaultTools() []ToolDefinition {
	return []ToolDefinition{
		ReadTool(),
		WriteTool(),
		EditTool(),
		GlobTool(),
		GrepTool(),
		BashTool(),
	}
}

// IsFileToolName reports whether name is one of Atteler's built-in in-process
// file tools.
func IsFileToolName(name string) bool {
	switch strings.TrimSpace(name) {
	case ToolNameRead, ToolNameWrite, ToolNameEdit, ToolNameGlob, ToolNameGrep:
		return true
	default:
		return false
	}
}

// IsWriteFileToolName reports whether name is a built-in file tool that can
// mutate the filesystem.
func IsWriteFileToolName(name string) bool {
	switch strings.TrimSpace(name) {
	case ToolNameWrite, ToolNameEdit:
		return true
	default:
		return false
	}
}

var (
	remoteScriptPattern         = regexp.MustCompile(`(?i)\b(curl|wget)\b[^\n;]*\|\s*(?:(?:sudo|command|env)\b(?:\s+[^\s|;&]+)*\s+)*(sh|bash|zsh)\b`)
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
	alwaysMutatingGitSubcommands = map[string]struct{}{
		dependencyActionAdd: {},
		"am":                {},
		"apply":             {},
		"cherry-pick":       {},
		"clean":             {},
		"clone":             {},
		"gc":                {},
		"init":              {},
		"maintenance":       {},
		gitActionMerge:      {},
		"mv":                {},
		commandActionPull:   {},
		"rebase":            {},
		"reset":             {},
		"restore":           {},
		"revert":            {},
		"rm":                {},
		"sparse-checkout":   {},
		"stash":             {},
		"submodule":         {},
		commandActionTag:    {},
		"update-index":      {},
		"update-ref":        {},
	}
)

const (
	// ToolNameBash is the built-in shell execution tool name.
	ToolNameBash = "bash"
	// ToolNameRead is the built-in file read tool name.
	ToolNameRead = "read"
	// ToolNameWrite is the built-in file write tool name.
	ToolNameWrite = "write"
	// ToolNameEdit is the built-in file edit tool name.
	ToolNameEdit = "edit"
	// ToolNameGlob is the built-in file glob tool name.
	ToolNameGlob = "glob"
	// ToolNameGrep is the built-in file grep tool name.
	ToolNameGrep = "grep"

	// FileToolMaxResults is the maximum max_results value accepted by the
	// built-in glob and grep file tools.
	FileToolMaxResults = 1000

	bashToolName = ToolNameBash

	dependencyActionAdd     = "add"
	dependencyActionInstall = "install"
	dependencyCommandGo     = "go"
	dependencyCommandNPM    = "npm"
	dependencyCommandPNPM   = "pnpm"
	dependencyCommandYarn   = "yarn"
	dependencyCommandPip    = "pip"
	dependencyCommandPoetry = "poetry"
	dependencyCommandCargo  = "cargo"
	dependencyCommandBrew   = "brew"
	dependencyCommandUV     = "uv"

	commandActionBuild   = "build"
	commandActionConfig  = "config"
	commandActionCreate  = "create"
	commandActionExec    = "exec"
	commandActionImport  = "import"
	commandActionPull    = "pull"
	commandActionPush    = "push"
	commandActionRestart = "restart"
	commandActionRun     = "run"
	commandActionStart   = "start"
	commandActionTag     = "tag"
	commandActionUpdate  = "update"
	packageActionPublish = "publish"

	commandCurl = "curl"
	commandWget = "wget"

	httpMethodDelete  = "DELETE"
	httpMethodGet     = "GET"
	httpMethodHead    = "HEAD"
	httpMethodOptions = "OPTIONS"
	httpMethodPatch   = "PATCH"
	httpMethodPost    = "POST"
	httpMethodPut     = "PUT"
	httpMethodTrace   = "TRACE"

	infrastructureNamespacePlugin = "plugin"
	infrastructureNamespaceRepo   = "repo"

	gitDeleteLongFlag = "--delete"
	gitListLongFlag   = "--list"

	gitActionEdit     = "edit"
	gitActionCheckout = "checkout"
	gitActionMerge    = "merge"
	gitActionPrune    = "prune"
	gitActionRemove   = "remove"

	maxBashAutonomyInspectionDepth = 8
)

// BashToolPolicy returns a conservative policy for the built-in bash tool. It
// allows ordinary read/build/test commands, denies obvious system-destructive
// commands, and requires confirmation for privileged or dependency-changing
// commands.
func BashToolPolicy(ctx context.Context, call ToolCall, _ AgentLoopBudgetSnapshot) ToolPolicyDecision {
	if call.Name != bashToolName {
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

	if decision, ok := bashToolPermissionPolicyDecision(ctx, command); ok {
		return decision
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

// DefaultToolPolicy returns a conservative policy for Atteler's built-in tools.
// It preserves the bash safety policy and adds first-class read/write/edit/glob
// and grep policy checks for the in-process file tools.
func DefaultToolPolicy(ctx context.Context, call ToolCall, budget AgentLoopBudgetSnapshot) ToolPolicyDecision {
	switch strings.TrimSpace(call.Name) {
	case ToolNameBash:
		return BashToolPolicy(ctx, call, budget)
	case ToolNameRead, ToolNameWrite, ToolNameEdit, ToolNameGlob, ToolNameGrep:
		return FileToolPolicy(ctx, call, budget)
	default:
		return ToolPolicyDecision{
			Verdict:     ToolPolicyDeny,
			Reason:      fmt.Sprintf("unknown tool %q", call.Name),
			MatchedRule: "tool.deny.unknown_tool",
		}
	}
}

// FileToolPolicy returns a policy decision for Atteler's built-in in-process
// file tools. Filesystem writes are capability-gated by autonomy in
// FileToolPolicyForAutonomy; this base policy validates the tool name and
// applies configured permission-policy prechecks.
func FileToolPolicy(ctx context.Context, call ToolCall, _ AgentLoopBudgetSnapshot) ToolPolicyDecision {
	if !IsFileToolName(call.Name) {
		return ToolPolicyDecision{
			Verdict:     ToolPolicyDeny,
			Reason:      fmt.Sprintf("unknown file tool %q", call.Name),
			MatchedRule: "file_tool.deny.unknown_tool",
		}
	}

	if decision, ok := fileToolPermissionPolicyDecision(ctx, call); ok {
		return decision
	}

	return ToolPolicyDecision{
		Verdict:     ToolPolicyAllow,
		Reason:      fmt.Sprintf("file tool %q passed built-in policy checks", strings.TrimSpace(call.Name)),
		MatchedRule: "file_tool.allow.default",
	}
}

// BashToolPolicyForAutonomy returns the built-in bash policy with additional
// hard capability boundaries for the selected autonomy level.
func BashToolPolicyForAutonomy(level autonomy.Level) ToolPolicy {
	level = autonomy.Normalize(level)

	return func(ctx context.Context, call ToolCall, budget AgentLoopBudgetSnapshot) ToolPolicyDecision {
		base := BashToolPolicy(ctx, call, budget)
		if base.Verdict == ToolPolicyDeny {
			return base
		}

		if call.Name == bashToolName {
			command, ok := call.Input["command"].(string)
			if !ok {
				return base
			}

			// Autonomy boundaries are hard capability limits. Check them even
			// when the base safety policy would otherwise ask for confirmation
			// so lower autonomy levels never downgrade a forbidden publish or
			// mutation into a confirmable action.
			if decision := BashAutonomyDecision(level, command); decision.Verdict == ToolPolicyDeny {
				return decision
			}
		}

		return base
	}
}

// DefaultToolPolicyForAutonomy returns the built-in policy for all default
// tools with hard autonomy boundaries applied.
func DefaultToolPolicyForAutonomy(level autonomy.Level) ToolPolicy {
	level = autonomy.Normalize(level)

	return func(ctx context.Context, call ToolCall, budget AgentLoopBudgetSnapshot) ToolPolicyDecision {
		if strings.TrimSpace(call.Name) == ToolNameBash {
			return BashToolPolicyForAutonomy(level)(ctx, call, budget)
		}

		if IsWriteFileToolName(call.Name) && !level.Allows(autonomy.ActionFileWrite) {
			return ToolPolicyDecision{
				Verdict:     ToolPolicyDeny,
				Reason:      autonomy.DenialMessage(level, autonomy.ActionFileWrite, strings.TrimSpace(call.Name)+" tool"),
				MatchedRule: "autonomy.deny.file_write",
			}
		}

		return DefaultToolPolicy(ctx, call, budget)
	}
}

// FileToolPolicyForAutonomy returns the policy for in-process file tools with
// hard autonomy boundaries applied.
func FileToolPolicyForAutonomy(level autonomy.Level) ToolPolicy {
	level = autonomy.Normalize(level)

	return func(ctx context.Context, call ToolCall, budget AgentLoopBudgetSnapshot) ToolPolicyDecision {
		if IsWriteFileToolName(call.Name) && !level.Allows(autonomy.ActionFileWrite) {
			return ToolPolicyDecision{
				Verdict:     ToolPolicyDeny,
				Reason:      autonomy.DenialMessage(level, autonomy.ActionFileWrite, strings.TrimSpace(call.Name)+" tool"),
				MatchedRule: "autonomy.deny.file_write",
			}
		}

		return FileToolPolicy(ctx, call, budget)
	}
}

// BashAutonomyDecision returns a deny verdict when command exceeds the selected
// autonomy level. It intentionally does not duplicate sensitive-operation
// confirmation checks from BashToolPolicy so callers can use it for explicit
// user shell commands as a lightweight autonomy preflight.
func BashAutonomyDecision(level autonomy.Level, command string) ToolPolicyDecision {
	level = autonomy.Normalize(level)

	finding, ok := classifyBashAutonomyAction(command)
	if !ok || level.Allows(finding.action) {
		return ToolPolicyDecision{
			Verdict:     ToolPolicyAllow,
			Reason:      "bash command passed autonomy checks",
			MatchedRule: "autonomy.allow",
		}
	}

	return ToolPolicyDecision{
		Verdict:     ToolPolicyDeny,
		Reason:      autonomy.DenialMessage(level, finding.action, finding.detail),
		MatchedRule: finding.rule,
	}
}

type bashAutonomyFinding struct {
	action autonomy.Action
	rule   string
	detail string
}

// BashCommandRequiresNetwork reports whether command is classified as requiring
// network capability by Atteler's permission parser.
func BashCommandRequiresNetwork(command string) bool {
	for _, op := range permission.CommandOperations(bashToolName, []string{"-lc", command}, command, "", "llm.bash_tool") {
		if op.Kind == permission.OperationNetwork {
			return true
		}
	}

	return false
}

// BashCommandRequiresWrite reports whether Atteler's shell classifier considers
// command outside read-only shell inspection. It is intentionally conservative:
// any mutating, remote, branch, commit, push, or PR action requires shell.write.
func BashCommandRequiresWrite(command string) bool {
	_, ok := classifyBashAutonomyAction(command)

	return ok
}

func classifyBashAutonomyAction(command string) (bashAutonomyFinding, bool) {
	return classifyBashAutonomyActionDepth(command, 0)
}

func classifyBashAutonomyActionDepth(command string, depth int) (bashAutonomyFinding, bool) {
	if remoteScriptPattern.MatchString(command) {
		return bashAutonomyFinding{action: autonomy.ActionMutatingShell, rule: "autonomy.deny.mutating_shell", detail: "remote shell script"}, true
	}

	if depth < maxBashAutonomyInspectionDepth {
		for _, script := range bashCommandSubstitutions(command) {
			if finding, ok := classifyBashAutonomyActionDepth(script, depth+1); ok {
				return finding, true
			}
		}
	}

	if strings.Contains(command, ">") {
		return bashAutonomyFinding{action: autonomy.ActionMutatingShell, rule: "autonomy.deny.mutating_shell", detail: "shell redirection"}, true
	}

	for _, fields := range bashCommandSegments(command) {
		fields = unwrapBashCommandFields(fields)
		if len(fields) == 0 {
			continue
		}

		if finding, ok := classifyBashAutonomySegment(fields); ok {
			return finding, true
		}
	}

	return bashAutonomyFinding{}, false
}

func classifyBashAutonomySegment(fields []string) (bashAutonomyFinding, bool) {
	name := strings.ToLower(fields[0])
	args := fields[1:]

	if finding, ok := classifyShellWrapperOrVCS(name, args); ok {
		return finding, true
	}

	if isDirectMutatingShellCommand(name) {
		return bashAutonomyFinding{action: autonomy.ActionMutatingShell, rule: "autonomy.deny.mutating_shell", detail: name}, true
	}

	if finding, ok := classifyHTTPRemoteCommand(name, args); ok {
		return finding, true
	}

	if finding, ok := classifyRemoteAccessCommand(name, args); ok {
		return finding, true
	}

	if finding, ok := classifyInPlaceEditCommand(name, args); ok {
		return finding, true
	}

	if ok, detail := makeCommandMutates(name, args); ok {
		return bashAutonomyFinding{action: autonomy.ActionMutatingShell, rule: "autonomy.deny.mutating_shell", detail: detail}, true
	}

	if finding, ok := classifyPackageRemoteCommand(name, args); ok {
		return finding, true
	}

	if finding, ok := classifyInfrastructureCommand(name, args); ok {
		return finding, true
	}

	if finding, ok := classifyCloudRemoteCommand(name, args); ok {
		return finding, true
	}

	if isDependencyCommand(name) && (dependencyCommandRequiresConfirmation(name, args) || goModCommandMutates(name, args)) {
		return bashAutonomyFinding{action: autonomy.ActionMutatingShell, rule: "autonomy.deny.mutating_shell", detail: name + " dependency change"}, true
	}

	if pythonPipCommandMutates(name, args) {
		return bashAutonomyFinding{action: autonomy.ActionMutatingShell, rule: "autonomy.deny.mutating_shell", detail: name + " pip dependency change"}, true
	}

	return bashAutonomyFinding{}, false
}

func classifyShellWrapperOrVCS(name string, args []string) (bashAutonomyFinding, bool) {
	switch name {
	case bashToolName, "sh", "zsh":
		if nested, ok := shellCommandArg(args); ok {
			return classifyBashAutonomyAction(nested)
		}

		return mutatingShellFindingIf(shellInterpreterRunsScript(args), name+" script")
	case "gh":
		return classifyGitHubCLIAction(stripGitHubGlobalOptions(args))
	case "git":
		return classifyGitAction(stripGitGlobalOptions(args))
	}

	return bashAutonomyFinding{}, false
}

func isDirectMutatingShellCommand(name string) bool {
	switch name {
	case "touch", "tee", "cp", "mv", "rm", "mkdir", "rmdir", "chmod", "chown", "truncate", "ln", dependencyActionInstall, "patch":
		return true
	default:
		return false
	}
}

func classifyInPlaceEditCommand(name string, args []string) (bashAutonomyFinding, bool) {
	switch name {
	case "sed":
		return mutatingShellFindingIf(sedCommandEditsInPlace(args), "sed -i")
	case "perl":
		return mutatingShellFindingIf(perlCommandEditsInPlace(args), "perl in-place edit")
	case "gofmt":
		return mutatingShellFindingIf(containsArg(args, "-w"), "gofmt -w")
	case "dd":
		return mutatingShellFindingIf(ddWritesOutput(args), "dd output")
	case commandCurl, commandWget:
		return mutatingShellFindingIf(downloadCommandWritesFile(name, args), name+" output file")
	case "tar":
		return mutatingShellFindingIf(tarCommandExtracts(args), "tar extract")
	case "unzip":
		return mutatingShellFindingIf(unzipCommandExtracts(args), "unzip extract")
	}

	return bashAutonomyFinding{}, false
}

func mutatingShellFindingIf(ok bool, detail string) (bashAutonomyFinding, bool) {
	if !ok {
		return bashAutonomyFinding{}, false
	}

	return bashAutonomyFinding{action: autonomy.ActionMutatingShell, rule: "autonomy.deny.mutating_shell", detail: detail}, true
}

func remoteMutationFindingIf(ok bool, detail string) (bashAutonomyFinding, bool) {
	if !ok {
		return bashAutonomyFinding{}, false
	}

	return bashAutonomyFinding{action: autonomy.ActionRemoteMutation, rule: "autonomy.deny.remote_mutation", detail: detail}, true
}

func makeCommandMutates(name string, args []string) (mutates bool, detail string) {
	if name != "make" {
		return false, ""
	}

	targets := makeCommandTargets(args)
	if len(targets) == 0 {
		return true, "make default target"
	}

	for _, target := range targets {
		if makeTargetMutates(target) {
			return true, "make " + target
		}
	}

	return false, ""
}

func makeCommandTargets(args []string) []string {
	targets := make([]string, 0, len(args))

	for i := 0; i < len(args); i++ {
		arg := strings.TrimSpace(args[i])
		if arg == "" || strings.Contains(arg, "=") {
			continue
		}

		if arg == "--" {
			targets = append(targets, args[i+1:]...)
			break
		}

		if strings.HasPrefix(arg, "-") {
			if makeOptionTakesValue(arg) && i+1 < len(args) {
				i++
			}

			continue
		}

		targets = append(targets, arg)
	}

	return targets
}

func makeOptionTakesValue(arg string) bool {
	arg = strings.ToLower(strings.TrimSpace(arg))
	switch arg {
	case "-c", "-f", "-i":
		return true
	default:
		return arg == "--directory" ||
			arg == "--file" ||
			arg == "--include-dir" ||
			arg == "--makefile"
	}
}

func makeTargetMutates(target string) bool {
	target = strings.ToLower(strings.TrimSpace(target))
	if target == "" {
		return false
	}

	for _, part := range strings.FieldsFunc(target, makeTargetSeparator) {
		switch part {
		case commandActionBuild, "rebuild", dependencyActionInstall, "generate", "gen", "codegen", "fmt", "format", "clean", "release", packageActionPublish, "deploy", "migrate", "migration", "migrations", "docker", "compose":
			return true
		}
	}

	return false
}

func makeTargetSeparator(r rune) bool {
	switch r {
	case '-', '_', ':', '/', '.':
		return true
	default:
		return false
	}
}

func classifyHTTPRemoteCommand(name string, args []string) (bashAutonomyFinding, bool) {
	switch name {
	case commandCurl:
		if curlCommandMergesPullRequest(args) {
			return bashAutonomyFinding{action: autonomy.ActionPullRequestMerge, rule: "autonomy.deny.pr_merge", detail: "curl pull request merge"}, true
		}

		return remoteMutationFindingIf(curlCommandMutatesRemote(args), "curl remote request")
	case commandWget:
		if wgetCommandMergesPullRequest(args) {
			return bashAutonomyFinding{action: autonomy.ActionPullRequestMerge, rule: "autonomy.deny.pr_merge", detail: "wget pull request merge"}, true
		}

		return remoteMutationFindingIf(wgetCommandMutatesRemote(args), "wget remote request")
	case "http", "https":
		if httpieCommandMergesPullRequest(args) {
			return bashAutonomyFinding{action: autonomy.ActionPullRequestMerge, rule: "autonomy.deny.pr_merge", detail: name + " pull request merge"}, true
		}

		return remoteMutationFindingIf(httpieCommandMutatesRemote(args), name+" remote request")
	default:
		return bashAutonomyFinding{}, false
	}
}

func classifyRemoteAccessCommand(name string, args []string) (bashAutonomyFinding, bool) {
	switch name {
	case "ssh":
		return remoteMutationFindingIf(sshCommandStartsRemoteSession(args), "ssh remote session")
	default:
		return bashAutonomyFinding{}, false
	}
}

func classifyPackageRemoteCommand(name string, args []string) (bashAutonomyFinding, bool) {
	switch name {
	case dependencyCommandNPM, dependencyCommandPNPM:
		return classifyNPMLikeRemoteCommand(name, args)
	case dependencyCommandYarn:
		return classifyYarnRemoteCommand(args)
	case dependencyCommandPoetry:
		subcommand := firstCommandSubcommandArg(stripPackageGlobalOptions(args))
		return remoteMutationFindingIf(subcommand == packageActionPublish, "poetry publish")
	case dependencyCommandCargo:
		return classifyCargoRemoteCommand(args)
	case "twine":
		subcommand := firstCommandSubcommandArg(stripPackageGlobalOptions(args))
		return remoteMutationFindingIf(subcommand == "upload", "twine upload")
	case "gem":
		subcommand := firstCommandSubcommandArg(stripPackageGlobalOptions(args))
		return remoteMutationFindingIf(dependencyActionIn(subcommand, commandActionPush, "yank"), "gem "+subcommand)
	default:
		return bashAutonomyFinding{}, false
	}
}

func classifyNPMLikeRemoteCommand(name string, args []string) (bashAutonomyFinding, bool) {
	args = stripPackageGlobalOptions(args)

	subcommand := firstCommandSubcommandArg(args)
	if subcommand == "" {
		return bashAutonomyFinding{}, false
	}

	switch subcommand {
	case packageActionPublish, "unpublish", "deprecate":
		return bashAutonomyFinding{action: autonomy.ActionRemoteMutation, rule: "autonomy.deny.remote_mutation", detail: name + " " + subcommand}, true
	case "dist-tag":
		return remoteMutationFindingIf(npmDistTagMutates(args[1:]), name+" dist-tag")
	case "access", "owner", "team", "token":
		return remoteMutationFindingIf(npmSecondaryCommandMutates(args[1:]), name+" "+subcommand)
	default:
		return bashAutonomyFinding{}, false
	}
}

func classifyYarnRemoteCommand(args []string) (bashAutonomyFinding, bool) {
	args = stripPackageGlobalOptions(args)

	subcommand := firstCommandSubcommandArg(args)
	if subcommand == "" {
		return bashAutonomyFinding{}, false
	}

	switch subcommand {
	case packageActionPublish:
		return bashAutonomyFinding{action: autonomy.ActionRemoteMutation, rule: "autonomy.deny.remote_mutation", detail: "yarn publish"}, true
	case "npm":
		return classifyYarnNPMRemoteCommand(args[1:])
	default:
		return bashAutonomyFinding{}, false
	}
}

func classifyYarnNPMRemoteCommand(args []string) (bashAutonomyFinding, bool) {
	args = stripPackageGlobalOptions(args)

	subcommand := firstCommandSubcommandArg(args)
	if subcommand == "" {
		return bashAutonomyFinding{}, false
	}

	switch subcommand {
	case packageActionPublish, "whoami":
		return remoteMutationFindingIf(subcommand == packageActionPublish, "yarn npm publish")
	case commandActionTag:
		return remoteMutationFindingIf(npmDistTagMutates(args[1:]), "yarn npm tag")
	case "access":
		return remoteMutationFindingIf(npmSecondaryCommandMutates(args[1:]), "yarn npm access")
	default:
		return bashAutonomyFinding{}, false
	}
}

func classifyCargoRemoteCommand(args []string) (bashAutonomyFinding, bool) {
	args = stripPackageGlobalOptions(args)

	subcommand := firstCommandSubcommandArg(args)
	if subcommand == "" {
		return bashAutonomyFinding{}, false
	}

	switch subcommand {
	case packageActionPublish, "yank":
		return bashAutonomyFinding{action: autonomy.ActionRemoteMutation, rule: "autonomy.deny.remote_mutation", detail: "cargo " + subcommand}, true
	case "owner":
		return remoteMutationFindingIf(npmSecondaryCommandMutates(args[1:]), "cargo owner")
	default:
		return bashAutonomyFinding{}, false
	}
}

func npmDistTagMutates(args []string) bool {
	subcommand := firstCommandSubcommandArg(stripPackageGlobalOptions(args))
	switch subcommand {
	case dependencyActionAdd, "rm", gitActionRemove:
		return true
	default:
		return false
	}
}

func npmSecondaryCommandMutates(args []string) bool {
	subcommand := firstCommandSubcommandArg(stripPackageGlobalOptions(args))
	if subcommand == "" {
		return false
	}

	switch subcommand {
	case "list", "ls", "whoami", "view":
		return false
	default:
		return true
	}
}

func classifyInfrastructureCommand(name string, args []string) (bashAutonomyFinding, bool) {
	switch name {
	case "docker":
		return classifyDockerCommand(args)
	case "kubectl":
		return classifyKubectlCommand(args)
	case "helm":
		return classifyHelmCommand(args)
	case "terraform":
		return classifyTerraformCommand(args)
	case "pulumi":
		return classifyPulumiCommand(args)
	default:
		return bashAutonomyFinding{}, false
	}
}

func classifyDockerCommand(args []string) (bashAutonomyFinding, bool) {
	args = stripDockerGlobalOptions(args)

	subcommand := firstCommandSubcommandArg(args)
	if subcommand == "" {
		return bashAutonomyFinding{}, false
	}

	if subcommand == "compose" {
		return classifyDockerComposeCommand(args[1:])
	}

	if subcommand == "buildx" {
		return classifyDockerBuildxCommand(args[1:])
	}

	if subcommand == "image" {
		return classifyDockerImageCommand(args[1:])
	}

	switch subcommand {
	case commandActionPush:
		return bashAutonomyFinding{action: autonomy.ActionRemoteMutation, rule: "autonomy.deny.remote_mutation", detail: "docker push"}, true
	case commandActionBuild, "builder", "commit", "container", "cp", commandActionCreate, commandActionExec, "login", "logout", "network", infrastructureNamespacePlugin, commandActionPull, "rename", commandActionRestart, "rm", "rmi", commandActionRun, "save", commandActionStart, string(AgentLoopStepStop), "system", commandActionTag, "volume":
		return bashAutonomyFinding{action: autonomy.ActionMutatingShell, rule: "autonomy.deny.mutating_shell", detail: "docker " + subcommand}, true
	default:
		return bashAutonomyFinding{}, false
	}
}

func classifyDockerComposeCommand(args []string) (bashAutonomyFinding, bool) {
	args = stripDockerComposeGlobalOptions(args)

	subcommand := firstCommandSubcommandArg(args)
	if subcommand == "" {
		return bashAutonomyFinding{}, false
	}

	switch subcommand {
	case commandActionPush:
		return bashAutonomyFinding{action: autonomy.ActionRemoteMutation, rule: "autonomy.deny.remote_mutation", detail: "docker compose push"}, true
	case commandActionBuild, "cp", commandActionCreate, "down", commandActionExec, "kill", "pause", commandActionPull, commandActionRestart, "rm", commandActionRun, commandActionStart, string(AgentLoopStepStop), "unpause", "up":
		return bashAutonomyFinding{action: autonomy.ActionMutatingShell, rule: "autonomy.deny.mutating_shell", detail: "docker compose " + subcommand}, true
	default:
		return bashAutonomyFinding{}, false
	}
}

func classifyDockerBuildxCommand(args []string) (bashAutonomyFinding, bool) {
	subcommand := firstCommandSubcommandArg(args)
	if subcommand == "" {
		return bashAutonomyFinding{}, false
	}

	switch subcommand {
	case commandActionBuild:
		if containsArg(args[1:], "--push") {
			return bashAutonomyFinding{action: autonomy.ActionRemoteMutation, rule: "autonomy.deny.remote_mutation", detail: "docker buildx build --push"}, true
		}

		return bashAutonomyFinding{action: autonomy.ActionMutatingShell, rule: "autonomy.deny.mutating_shell", detail: "docker buildx build"}, true
	case "imagetools":
		return classifyDockerBuildxImageToolsCommand(args[1:])
	case commandActionCreate, "du", gitActionPrune, "rm", string(AgentLoopStepStop), "use":
		return bashAutonomyFinding{action: autonomy.ActionMutatingShell, rule: "autonomy.deny.mutating_shell", detail: "docker buildx " + subcommand}, true
	default:
		return bashAutonomyFinding{}, false
	}
}

func classifyDockerBuildxImageToolsCommand(args []string) (bashAutonomyFinding, bool) {
	subcommand := firstCommandSubcommandArg(args)
	if subcommand == "" {
		return bashAutonomyFinding{}, false
	}

	switch subcommand {
	case commandActionCreate:
		return bashAutonomyFinding{action: autonomy.ActionRemoteMutation, rule: "autonomy.deny.remote_mutation", detail: "docker buildx imagetools create"}, true
	default:
		return bashAutonomyFinding{}, false
	}
}

func classifyDockerImageCommand(args []string) (bashAutonomyFinding, bool) {
	subcommand := firstCommandSubcommandArg(args)
	if subcommand == "" {
		return bashAutonomyFinding{}, false
	}

	switch subcommand {
	case commandActionPush:
		return bashAutonomyFinding{action: autonomy.ActionRemoteMutation, rule: "autonomy.deny.remote_mutation", detail: "docker image push"}, true
	case commandActionBuild, commandActionImport, "load", gitActionPrune, commandActionPull, "rm", commandActionTag:
		return bashAutonomyFinding{action: autonomy.ActionMutatingShell, rule: "autonomy.deny.mutating_shell", detail: "docker image " + subcommand}, true
	default:
		return bashAutonomyFinding{}, false
	}
}

func classifyKubectlCommand(args []string) (bashAutonomyFinding, bool) {
	args = stripKubectlGlobalOptions(args)

	subcommand := firstCommandSubcommandArg(args)
	if subcommand == "" {
		return bashAutonomyFinding{}, false
	}

	switch subcommand {
	case "apply", "annotate", "autoscale", "cordon", commandActionCreate, "delete", "drain", "edit", commandActionExec, "expose", "label", "patch", "replace", "rollout", commandActionRun, "scale", "set", "taint", "uncordon":
		return bashAutonomyFinding{action: autonomy.ActionRemoteMutation, rule: "autonomy.deny.remote_mutation", detail: "kubectl " + subcommand}, true
	case commandActionConfig:
		return classifyKubectlConfigCommand(args[1:])
	case "cp", "port-forward", "proxy":
		return bashAutonomyFinding{action: autonomy.ActionMutatingShell, rule: "autonomy.deny.mutating_shell", detail: "kubectl " + subcommand}, true
	default:
		return bashAutonomyFinding{}, false
	}
}

func classifyKubectlConfigCommand(args []string) (bashAutonomyFinding, bool) {
	subcommand := firstCommandSubcommandArg(stripKubectlGlobalOptions(args))
	if subcommand == "" {
		return bashAutonomyFinding{}, false
	}

	switch subcommand {
	case "current-context", "get-contexts", "view":
		return bashAutonomyFinding{}, false
	default:
		return bashAutonomyFinding{action: autonomy.ActionMutatingShell, rule: "autonomy.deny.mutating_shell", detail: "kubectl config " + subcommand}, true
	}
}

func classifyHelmCommand(args []string) (bashAutonomyFinding, bool) {
	args = stripHelmGlobalOptions(args)

	subcommand := firstCommandSubcommandArg(args)
	if subcommand == "" {
		return bashAutonomyFinding{}, false
	}

	switch subcommand {
	case dependencyActionInstall, "rollback", "test", "uninstall", "upgrade":
		return bashAutonomyFinding{action: autonomy.ActionRemoteMutation, rule: "autonomy.deny.remote_mutation", detail: "helm " + subcommand}, true
	case commandActionPush:
		return bashAutonomyFinding{action: autonomy.ActionRemoteMutation, rule: "autonomy.deny.remote_mutation", detail: "helm push"}, true
	case "dependency", infrastructureNamespacePlugin, infrastructureNamespaceRepo:
		return classifyHelmLocalNamespaceCommand(subcommand, args[1:])
	default:
		return bashAutonomyFinding{}, false
	}
}

func classifyHelmLocalNamespaceCommand(namespace string, args []string) (bashAutonomyFinding, bool) {
	subcommand := firstCommandSubcommandArg(stripHelmGlobalOptions(args))
	if subcommand == "" {
		return bashAutonomyFinding{}, false
	}

	switch namespace {
	case "dependency":
		return mutatingShellFindingIf(dependencyActionIn(subcommand, commandActionBuild, commandActionUpdate), "helm dependency "+subcommand)
	case infrastructureNamespacePlugin:
		return mutatingShellFindingIf(dependencyActionIn(subcommand, dependencyActionInstall, gitActionRemove, "uninstall", commandActionUpdate), "helm plugin "+subcommand)
	case infrastructureNamespaceRepo:
		return mutatingShellFindingIf(dependencyActionIn(subcommand, dependencyActionAdd, gitActionRemove, commandActionUpdate), "helm repo "+subcommand)
	default:
		return bashAutonomyFinding{}, false
	}
}

func classifyTerraformCommand(args []string) (bashAutonomyFinding, bool) {
	args = stripTerraformGlobalOptions(args)

	subcommand := firstCommandSubcommandArg(args)
	if subcommand == "" {
		return bashAutonomyFinding{}, false
	}

	switch subcommand {
	case "apply", "destroy", "force-unlock", commandActionImport, "refresh":
		return bashAutonomyFinding{action: autonomy.ActionRemoteMutation, rule: "autonomy.deny.remote_mutation", detail: "terraform " + subcommand}, true
	case "init", "state", "taint", "untaint", "workspace":
		return bashAutonomyFinding{action: autonomy.ActionMutatingShell, rule: "autonomy.deny.mutating_shell", detail: "terraform " + subcommand}, true
	case "fmt":
		return mutatingShellFindingIf(!containsArg(args[1:], "-check") && !containsArg(args[1:], "--check"), "terraform fmt")
	case "plan":
		return mutatingShellFindingIf(terraformPlanWritesFile(args[1:]), "terraform plan -out")
	default:
		return bashAutonomyFinding{}, false
	}
}

func classifyPulumiCommand(args []string) (bashAutonomyFinding, bool) {
	args = stripPulumiGlobalOptions(args)

	subcommand := firstCommandSubcommandArg(args)
	if subcommand == "" {
		return bashAutonomyFinding{}, false
	}

	switch subcommand {
	case "cancel", "destroy", commandActionImport, "refresh", "up":
		return bashAutonomyFinding{action: autonomy.ActionRemoteMutation, rule: "autonomy.deny.remote_mutation", detail: "pulumi " + subcommand}, true
	case "login", "logout", "new", "stack":
		return bashAutonomyFinding{action: autonomy.ActionMutatingShell, rule: "autonomy.deny.mutating_shell", detail: "pulumi " + subcommand}, true
	default:
		return bashAutonomyFinding{}, false
	}
}

func classifyCloudRemoteCommand(name string, args []string) (bashAutonomyFinding, bool) {
	switch name {
	case "aws":
		return classifyAWSCommand(args)
	case "gcloud":
		return classifyGCloudCommand(args)
	case "az":
		return classifyAzureCommand(args)
	case "firebase":
		return classifyFirebaseCommand(args)
	case "vercel", "netlify", "fly", "railway", "heroku", "supabase":
		return classifyDeploymentCLICommand(name, args)
	default:
		return bashAutonomyFinding{}, false
	}
}

func classifyAWSCommand(args []string) (bashAutonomyFinding, bool) {
	parts := cloudCommandParts(stripAWSGlobalOptions(args), 2)
	if len(parts) < 2 {
		return bashAutonomyFinding{}, false
	}

	service, operation := parts[0], parts[1]
	mutating := map[string][]string{
		"cloudformation": {"deploy", "delete-stack", "update-stack", "create-stack"},
		"ecr":            {"put-image", "delete-repository", "batch-delete-image", "create-repository"},
		"ecs":            {"update-service", "create-service", "delete-service", "run-task"},
		"eks":            {"create-cluster", "delete-cluster", "update-cluster-config", "update-kubeconfig"},
		"iam":            {"create", "delete", "attach", "detach", "put", "update"},
		"lambda":         {"create-function", "delete-function", "update-function-code", "update-function-configuration", "publish-version"},
		"route53":        {"change-resource-record-sets"},
		"s3":             {"cp", "sync", "rm", "mb", "rb", "mv"},
		"s3api":          {"put", "delete", "create", "copy", "restore"},
		"secretsmanager": {"create-secret", "delete-secret", "put-secret-value", "update-secret"},
		"ssm":            {"put-parameter", "delete-parameter", "send-command", "start-automation-execution"},
	}

	return remoteMutationFindingIf(cloudOperationHasPrefix(operation, mutating[service]), "aws "+service+" "+operation)
}

func classifyGCloudCommand(args []string) (bashAutonomyFinding, bool) {
	parts := cloudCommandParts(stripGCloudGlobalOptions(args), 3)
	if len(parts) < 2 {
		return bashAutonomyFinding{}, false
	}

	return remoteMutationFindingIf(cloudPartsMatch(parts,
		[]string{"app", "deploy"},
		[]string{"app", "delete"},
		[]string{"builds", "submit"},
		[]string{"compute", "instances", "create"},
		[]string{"compute", "instances", "delete"},
		[]string{"compute", "instances", "reset"},
		[]string{"compute", "instances", "start"},
		[]string{"compute", "instances", "stop"},
		[]string{"compute", "instances", commandActionUpdate},
		[]string{"container", "clusters", "create"},
		[]string{"container", "clusters", "delete"},
		[]string{"container", "clusters", commandActionUpdate},
		[]string{"container", "clusters", "upgrade"},
		[]string{"deploy", "releases", "create"},
		[]string{"functions", "delete"},
		[]string{"functions", "deploy"},
		[]string{"run", "delete"},
		[]string{"run", "deploy"},
		[]string{"run", "jobs", "create"},
		[]string{"run", "jobs", "delete"},
		[]string{"run", "jobs", "execute"},
		[]string{"run", "jobs", commandActionUpdate},
		[]string{"secrets", "versions", "add"},
		[]string{"sql", "instances", "create"},
		[]string{"sql", "instances", "delete"},
		[]string{"sql", "instances", "patch"},
		[]string{"sql", "instances", commandActionRestart},
	), "gcloud "+strings.Join(parts, " "))
}

func classifyAzureCommand(args []string) (bashAutonomyFinding, bool) {
	parts := cloudCommandParts(stripAzureGlobalOptions(args), 3)
	if len(parts) < 2 {
		return bashAutonomyFinding{}, false
	}

	return remoteMutationFindingIf(cloudPartsMatch(parts,
		[]string{"acr", commandActionBuild},
		[]string{"acr", "create"},
		[]string{"acr", "delete"},
		[]string{"acr", commandActionImport},
		[]string{"acr", commandActionUpdate},
		[]string{"containerapp", "create"},
		[]string{"containerapp", "delete"},
		[]string{"containerapp", commandActionUpdate},
		[]string{"deployment", "cancel"},
		[]string{"deployment", "create"},
		[]string{"deployment", "delete"},
		[]string{"functionapp", "deploy"},
		[]string{"group", "create"},
		[]string{"group", "delete"},
		[]string{"keyvault", "create"},
		[]string{"keyvault", "delete"},
		[]string{"role", "assignment"},
		[]string{"webapp", "deploy"},
		[]string{"webapp", commandActionRestart},
	), "az "+strings.Join(parts, " "))
}

func classifyFirebaseCommand(args []string) (bashAutonomyFinding, bool) {
	parts := cloudCommandParts(stripGenericCloudGlobalOptions(args), 2)
	if len(parts) == 0 {
		return bashAutonomyFinding{}, false
	}

	return remoteMutationFindingIf(cloudPartIn(parts[0], "deploy", "hosting:disable", "functions:delete", "database:set", "firestore:delete"), "firebase "+strings.Join(parts, " "))
}

func classifyDeploymentCLICommand(name string, args []string) (bashAutonomyFinding, bool) {
	parts := cloudCommandParts(stripGenericCloudGlobalOptions(args), 2)
	if len(parts) == 0 {
		return remoteMutationFindingIf(name == "vercel", name+" deploy")
	}

	mutates := cloudPartIn(parts[0], "deploy", "up", "launch", "rollback", "destroy", "delete", "remove", "rm")
	if !mutates && len(parts) >= 2 {
		switch parts[0] {
		case "alias", "apps", "builds", commandActionConfig, "domains", "env", "secrets", "vars", "variables":
			mutates = cloudPartIn(parts[1], "add", "create", "delete", "destroy", "link", "remove", "rm", "set", "unset", "update")
		}
	}

	return remoteMutationFindingIf(mutates, name+" "+strings.Join(parts, " "))
}

func cloudCommandParts(args []string, maxParts int) []string {
	parts := make([]string, 0, maxParts)

	for _, arg := range args {
		arg = strings.ToLower(strings.TrimSpace(arg))
		if arg == "" || arg == "--" || strings.HasPrefix(arg, "-") {
			continue
		}

		parts = append(parts, arg)
		if len(parts) >= maxParts {
			return parts
		}
	}

	return parts
}

func cloudOperationHasPrefix(operation string, prefixes []string) bool {
	for _, prefix := range prefixes {
		if operation == prefix || strings.HasPrefix(operation, prefix+"-") {
			return true
		}
	}

	return false
}

func cloudPartsMatch(parts []string, patterns ...[]string) bool {
	for _, pattern := range patterns {
		if len(parts) < len(pattern) {
			continue
		}

		matched := true

		for i := range pattern {
			if parts[i] != pattern[i] {
				matched = false

				break
			}
		}

		if matched {
			return true
		}
	}

	return false
}

func cloudPartIn(part string, values ...string) bool {
	return slices.Contains(values, part)
}

func terraformPlanWritesFile(args []string) bool {
	for i, arg := range args {
		arg = strings.TrimSpace(arg)
		if arg == "" {
			continue
		}

		lower := strings.ToLower(arg)
		if lower == "-out" {
			return i+1 < len(args) && strings.TrimSpace(args[i+1]) != ""
		}

		if strings.HasPrefix(lower, "-out=") || strings.HasPrefix(lower, "--out=") {
			return true
		}
	}

	return false
}

//nolint:cyclop,gocognit // Quote-aware command-substitution scanning is intentionally local and auditable.
func bashCommandSubstitutions(command string) []string {
	scripts := make([]string, 0)

	var (
		quote   byte
		escaped bool
	)

	for i := 0; i < len(command); i++ {
		c := command[i]

		if escaped {
			escaped = false
			continue
		}

		if c == '\\' && quote != '\'' {
			escaped = true
			continue
		}

		if quote != 0 {
			if c == quote {
				quote = 0
				continue
			}

			if quote == '\'' {
				continue
			}
		} else if c == '\'' || c == '"' {
			quote = c
			continue
		}

		if c == '`' {
			end, ok := closingBashBacktick(command, i+1)
			if !ok {
				continue
			}

			scripts = append(scripts, command[i+1:end])
			i = end

			continue
		}

		if c == '$' && i+1 < len(command) && command[i+1] == '(' {
			end, ok := closingBashDollarParen(command, i+2)
			if !ok {
				continue
			}

			scripts = append(scripts, command[i+2:end])
			i = end
		}

		if (c == '<' || c == '>') && i+1 < len(command) && command[i+1] == '(' {
			end, ok := closingBashDollarParen(command, i+2)
			if !ok {
				continue
			}

			scripts = append(scripts, command[i+2:end])
			i = end
		}
	}

	return scripts
}

func closingBashBacktick(command string, start int) (int, bool) {
	escaped := false

	for i := start; i < len(command); i++ {
		c := command[i]

		if escaped {
			escaped = false
			continue
		}

		if c == '\\' {
			escaped = true
			continue
		}

		if c == '`' {
			return i, true
		}
	}

	return 0, false
}

func closingBashDollarParen(command string, start int) (int, bool) {
	depth := 1

	var (
		quote   byte
		escaped bool
	)

	for i := start; i < len(command); i++ {
		c := command[i]

		if escaped {
			escaped = false
			continue
		}

		if c == '\\' && quote != '\'' {
			escaped = true
			continue
		}

		if quote != 0 {
			if c == quote {
				quote = 0
			}

			continue
		}

		switch c {
		case '\'', '"':
			quote = c
		case '$':
			if i+1 < len(command) && command[i+1] == '(' {
				depth++
				i++
			}
		case '(':
			depth++
		case ')':
			depth--
			if depth == 0 {
				return i, true
			}
		}
	}

	return 0, false
}

func isDependencyCommand(name string) bool {
	switch name {
	case dependencyCommandGo, dependencyCommandNPM, dependencyCommandPNPM, dependencyCommandYarn, dependencyCommandPip, dependencyCommandPoetry, dependencyCommandCargo, dependencyCommandBrew, dependencyCommandUV:
		return true
	default:
		return false
	}
}

func classifyGitHubCLIAction(args []string) (bashAutonomyFinding, bool) {
	if len(args) >= 2 && strings.EqualFold(args[0], "pr") {
		switch strings.ToLower(args[1]) {
		case gitActionMerge:
			return bashAutonomyFinding{action: autonomy.ActionPullRequestMerge, rule: "autonomy.deny.pr_merge", detail: "gh pr merge"}, true
		case commandActionCreate:
			return bashAutonomyFinding{action: autonomy.ActionPullRequestCreate, rule: "autonomy.deny.pr_create", detail: "gh pr create"}, true
		case gitActionCheckout:
			return bashAutonomyFinding{action: autonomy.ActionBranch, rule: "autonomy.deny.branch", detail: "gh pr checkout"}, true
		}
	}

	if len(args) > 0 && strings.EqualFold(args[0], "api") {
		apiArgs := args[1:]
		if githubAPIArgsMergePullRequest(apiArgs) {
			return bashAutonomyFinding{action: autonomy.ActionPullRequestMerge, rule: "autonomy.deny.pr_merge", detail: "gh api pull request merge"}, true
		}

		if githubAPIArgsCreatePullRequest(apiArgs) {
			return bashAutonomyFinding{action: autonomy.ActionPullRequestCreate, rule: "autonomy.deny.pr_create", detail: "gh api pull request create"}, true
		}

		if githubAPIArgsOpaqueGraphQLMutation(apiArgs) {
			return bashAutonomyFinding{action: autonomy.ActionPullRequestMerge, rule: "autonomy.deny.pr_merge", detail: "gh api graphql opaque mutation"}, true
		}

		if githubAPIMethodMutates(apiArgs) {
			return bashAutonomyFinding{action: autonomy.ActionRemoteMutation, rule: "autonomy.deny.remote_mutation", detail: "gh api " + strings.ToLower(githubAPIMethod(apiArgs))}, true
		}
	}

	if len(args) >= 2 {
		return classifyGitHubCLISubcommandAction(args[0], args[1])
	}

	return bashAutonomyFinding{}, false
}

func classifyGitHubCLISubcommandAction(scope, subcommand string) (bashAutonomyFinding, bool) {
	scope = strings.ToLower(strings.TrimSpace(scope))
	subcommand = strings.ToLower(strings.TrimSpace(subcommand))

	if scope == "" || subcommand == "" {
		return bashAutonomyFinding{}, false
	}

	readOnly := map[string][]string{
		"alias":                     {"list"},
		"auth":                      {"status"},
		"cache":                     {"list"},
		commandActionConfig:         {"get", "list"},
		"codespace":                 {"code", "cp", "jupyter", "list", "logs", "ports", "ssh", "view"},
		"extension":                 {"list"},
		"gist":                      {"list", "view"},
		"gpg-key":                   {"list"},
		"issue":                     {"list", "status", "view"},
		"label":                     {"list"},
		"pr":                        {"checks", "diff", "list", "status", "view"},
		"project":                   {"field-list", "item-list", "list", "view"},
		"release":                   {"list", "view"},
		infrastructureNamespaceRepo: {"list", "view"},
		"ruleset":                   {"check", "list", "view"},
		commandActionRun:            {"list", "view", "watch"},
		"secret":                    {"list"},
		"ssh-key":                   {"list"},
		"variable":                  {"list"},
		"workflow":                  {"list", "view"},
	}

	allowedReadOnly, ok := readOnly[scope]
	if !ok {
		return bashAutonomyFinding{}, false
	}

	if slices.Contains(allowedReadOnly, subcommand) {
		return bashAutonomyFinding{}, false
	}

	action := autonomy.ActionMutatingShell
	rule := "autonomy.deny.mutating_shell"

	if githubCLIScopeMutatesRemote(scope) {
		action = autonomy.ActionRemoteMutation
		rule = "autonomy.deny.remote_mutation"
	}

	return bashAutonomyFinding{
		action: action,
		rule:   rule,
		detail: "gh " + scope + " " + subcommand,
	}, true
}

func githubCLIScopeMutatesRemote(scope string) bool {
	switch strings.ToLower(strings.TrimSpace(scope)) {
	case "cache",
		"codespace",
		"gist",
		"gpg-key",
		"issue",
		"label",
		"pr",
		"project",
		"release",
		infrastructureNamespaceRepo,
		"ruleset",
		commandActionRun,
		"secret",
		"ssh-key",
		"variable",
		"workflow":
		return true
	default:
		return false
	}
}

func classifyGitAction(args []string) (bashAutonomyFinding, bool) {
	args = stripGitGlobalOptions(args)
	if len(args) == 0 {
		return bashAutonomyFinding{}, false
	}

	subcommand := strings.ToLower(args[0])
	if finding, ok := classifyGitPublishingAction(subcommand, args[1:]); ok {
		return finding, true
	}

	if finding, ok := classifyGitSpecialAction(subcommand, args[1:]); ok {
		return finding, true
	}

	if _, ok := alwaysMutatingGitSubcommands[subcommand]; ok {
		return bashAutonomyFinding{action: autonomy.ActionMutatingShell, rule: "autonomy.deny.mutating_shell", detail: "git " + subcommand}, true
	}

	return bashAutonomyFinding{}, false
}

func classifyGitPublishingAction(subcommand string, args []string) (bashAutonomyFinding, bool) {
	switch subcommand {
	case "am", "commit-tree", "rebase":
		return gitCommitFinding("git " + subcommand), true
	case "cherry-pick", "revert":
		return classifyGitSequencerCommitAction(subcommand, args)
	case "commit":
		return gitCommitFinding("git commit"), true
	case commandActionPush:
		return bashAutonomyFinding{action: autonomy.ActionPush, rule: "autonomy.deny.git_push", detail: "git push"}, true
	case gitActionMerge:
		return classifyGitMergeAction(args)
	case gitActionCheckout:
		return classifyGitCheckoutAction(args)
	case "switch":
		return classifyGitSwitchAction(args)
	case "branch":
		if gitBranchMutates(args) {
			return bashAutonomyFinding{action: autonomy.ActionMutatingShell, rule: "autonomy.deny.mutating_shell", detail: "git branch"}, true
		}

		if gitBranchCreates(args) {
			return bashAutonomyFinding{action: autonomy.ActionBranch, rule: "autonomy.deny.branch", detail: "git branch"}, true
		}
	}

	return bashAutonomyFinding{}, false
}

func gitCommitFinding(detail string) bashAutonomyFinding {
	return bashAutonomyFinding{action: autonomy.ActionCommit, rule: "autonomy.deny.git_commit", detail: detail}
}

func classifyGitMergeAction(args []string) (bashAutonomyFinding, bool) {
	if gitMergeAvoidsCommit(args) {
		return bashAutonomyFinding{action: autonomy.ActionMutatingShell, rule: "autonomy.deny.mutating_shell", detail: "git merge"}, true
	}

	return gitCommitFinding("git merge"), true
}

func classifyGitSequencerCommitAction(subcommand string, args []string) (bashAutonomyFinding, bool) {
	if gitSequencerAvoidsCommit(args) {
		return bashAutonomyFinding{action: autonomy.ActionMutatingShell, rule: "autonomy.deny.mutating_shell", detail: "git " + subcommand}, true
	}

	return gitCommitFinding("git " + subcommand), true
}

func classifyGitSpecialAction(subcommand string, args []string) (bashAutonomyFinding, bool) {
	switch subcommand {
	case commandActionConfig:
		return classifyGitConfigAction(args)
	case "remote":
		return classifyGitRemoteAction(args)
	case "worktree":
		return classifyGitWorktreeAction(args)
	case "notes":
		return classifyGitNotesAction(args)
	case "replace":
		return classifyGitReplaceAction(args)
	case "fetch":
		return classifyGitFetchAction(args)
	case "reflog":
		return classifyGitReflogAction(args)
	case "symbolic-ref":
		return classifyGitSymbolicRefAction(args)
	case "update-ref":
		return classifyGitUpdateRefAction(args)
	case "lfs":
		return classifyGitLFSAction(args)
	}

	return bashAutonomyFinding{}, false
}

func classifyGitConfigAction(args []string) (bashAutonomyFinding, bool) {
	if gitConfigMutates(args) {
		return bashAutonomyFinding{action: autonomy.ActionMutatingShell, rule: "autonomy.deny.mutating_shell", detail: "git config"}, true
	}

	return bashAutonomyFinding{}, false
}

func classifyGitRemoteAction(args []string) (bashAutonomyFinding, bool) {
	subcommand := firstGitSubcommandArg(args)
	switch subcommand {
	case dependencyActionAdd, gitActionRemove, "rm", "rename", "set-branches", "set-head", "set-url", gitActionPrune, commandActionUpdate:
		return bashAutonomyFinding{action: autonomy.ActionMutatingShell, rule: "autonomy.deny.mutating_shell", detail: "git remote " + subcommand}, true
	default:
		return bashAutonomyFinding{}, false
	}
}

func classifyGitWorktreeAction(args []string) (bashAutonomyFinding, bool) {
	subcommand := firstGitSubcommandArg(args)
	switch subcommand {
	case dependencyActionAdd:
		return bashAutonomyFinding{action: autonomy.ActionBranch, rule: "autonomy.deny.branch", detail: "git worktree add"}, true
	case "lock", "move", "mv", gitActionPrune, gitActionRemove, "repair", "rm", "unlock":
		return bashAutonomyFinding{action: autonomy.ActionMutatingShell, rule: "autonomy.deny.mutating_shell", detail: "git worktree " + subcommand}, true
	default:
		return bashAutonomyFinding{}, false
	}
}

func classifyGitNotesAction(args []string) (bashAutonomyFinding, bool) {
	subcommand := firstGitSubcommandArg(stripGitNotesGlobalOptions(args))
	switch subcommand {
	case dependencyActionAdd, "append", "copy", gitActionEdit, gitActionMerge, gitActionPrune, gitActionRemove, "rm":
		return bashAutonomyFinding{action: autonomy.ActionMutatingShell, rule: "autonomy.deny.mutating_shell", detail: "git notes " + subcommand}, true
	default:
		return bashAutonomyFinding{}, false
	}
}

func classifyGitReplaceAction(args []string) (bashAutonomyFinding, bool) {
	if gitReplaceMutates(args) {
		return bashAutonomyFinding{action: autonomy.ActionMutatingShell, rule: "autonomy.deny.mutating_shell", detail: "git replace"}, true
	}

	return bashAutonomyFinding{}, false
}

func classifyGitFetchAction(args []string) (bashAutonomyFinding, bool) {
	if containsArg(args, "--dry-run") {
		return bashAutonomyFinding{}, false
	}

	return bashAutonomyFinding{action: autonomy.ActionMutatingShell, rule: "autonomy.deny.mutating_shell", detail: "git fetch"}, true
}

func classifyGitReflogAction(args []string) (bashAutonomyFinding, bool) {
	subcommand := firstGitSubcommandArg(args)
	switch subcommand {
	case "delete", "expire":
		return bashAutonomyFinding{action: autonomy.ActionMutatingShell, rule: "autonomy.deny.mutating_shell", detail: "git reflog " + subcommand}, true
	default:
		return bashAutonomyFinding{}, false
	}
}

func classifyGitSymbolicRefAction(args []string) (bashAutonomyFinding, bool) {
	if gitSymbolicRefMutates(args) {
		return bashAutonomyFinding{action: autonomy.ActionMutatingShell, rule: "autonomy.deny.mutating_shell", detail: "git symbolic-ref"}, true
	}

	return bashAutonomyFinding{}, false
}

func classifyGitUpdateRefAction(args []string) (bashAutonomyFinding, bool) {
	if gitUpdateRefUsesStdin(args) {
		return bashAutonomyFinding{action: autonomy.ActionBranch, rule: "autonomy.deny.branch", detail: "git update-ref --stdin"}, true
	}

	ref, deleting := gitUpdateRefTarget(args)
	if deleting {
		return bashAutonomyFinding{action: autonomy.ActionMutatingShell, rule: "autonomy.deny.mutating_shell", detail: "git update-ref delete"}, true
	}

	if gitRefIsBranch(ref) {
		return bashAutonomyFinding{action: autonomy.ActionBranch, rule: "autonomy.deny.branch", detail: "git update-ref " + ref}, true
	}

	if ref != "" {
		return bashAutonomyFinding{action: autonomy.ActionMutatingShell, rule: "autonomy.deny.mutating_shell", detail: "git update-ref"}, true
	}

	return bashAutonomyFinding{}, false
}

func classifyGitLFSAction(args []string) (bashAutonomyFinding, bool) {
	subcommand := firstGitSubcommandArg(args)
	switch subcommand {
	case "checkout", "fetch", dependencyActionInstall, "migrate", gitActionPrune, commandActionPull, "track", "uninstall", "untrack", commandActionUpdate:
		return bashAutonomyFinding{action: autonomy.ActionMutatingShell, rule: "autonomy.deny.mutating_shell", detail: "git lfs " + subcommand}, true
	default:
		return bashAutonomyFinding{}, false
	}
}

func classifyGitCheckoutAction(args []string) (bashAutonomyFinding, bool) {
	if gitCheckoutCreatesBranch(args) {
		return bashAutonomyFinding{action: autonomy.ActionBranch, rule: "autonomy.deny.branch", detail: "git checkout branch creation"}, true
	}

	return bashAutonomyFinding{action: autonomy.ActionMutatingShell, rule: "autonomy.deny.mutating_shell", detail: "git checkout"}, true
}

func classifyGitSwitchAction(args []string) (bashAutonomyFinding, bool) {
	if gitSwitchCreatesBranch(args) {
		return bashAutonomyFinding{action: autonomy.ActionBranch, rule: "autonomy.deny.branch", detail: "git switch --create"}, true
	}

	return bashAutonomyFinding{action: autonomy.ActionMutatingShell, rule: "autonomy.deny.mutating_shell", detail: "git switch"}, true
}

func bashToolPermissionPolicyDecision(ctx context.Context, command string) (ToolPolicyDecision, bool) {
	policy := permission.PolicyFromContext(ctx)
	if policy == nil {
		return ToolPolicyDecision{}, false
	}

	ops := permission.CommandOperations("bash", []string{"-lc", command}, command, "", "llm.bash_tool")
	if !bashToolNeedsPermissionPrecheck(ctx, policy, ops) {
		return ToolPolicyDecision{}, false
	}

	decision := permission.Evaluate(ctx, policy, permission.Request{
		Action:     command,
		Source:     "llm.bash_tool",
		Target:     "bash",
		Operations: ops,
	})
	if decision.Allowed {
		return ToolPolicyDecision{}, false
	}

	return ToolPolicyDecision{
		Verdict:     ToolPolicyDeny,
		Reason:      decision.Reason,
		MatchedRule: decision.Rule,
	}, true
}

func fileToolPermissionPolicyDecision(ctx context.Context, call ToolCall) (ToolPolicyDecision, bool) {
	policy := permission.PolicyFromContext(ctx)
	if policy == nil {
		return ToolPolicyDecision{}, false
	}

	ops, ok := fileToolPermissionOperations(call)
	if !ok {
		return ToolPolicyDecision{}, false
	}

	if !fileToolNeedsPermissionPrecheck(ctx, policy, ops) {
		return ToolPolicyDecision{}, false
	}

	request := permission.Request{
		Action:     ops[0].Action,
		Source:     ops[0].Source,
		Target:     ops[0].Target,
		Operations: ops,
	}

	decision := permission.Evaluate(ctx, policy, request)
	if decision.Allowed {
		return ToolPolicyDecision{}, false
	}

	return ToolPolicyDecision{
		Verdict:     ToolPolicyDeny,
		Reason:      decision.Reason,
		MatchedRule: decision.Rule,
	}, true
}

func fileToolPermissionOperations(call ToolCall) ([]permission.Operation, bool) {
	toolName := strings.TrimSpace(call.Name)
	if !IsFileToolName(toolName) {
		return nil, false
	}

	kinds := []permission.OperationKind{permission.OperationRead}

	switch toolName {
	case ToolNameWrite:
		kinds = []permission.OperationKind{permission.OperationWrite}
	case ToolNameEdit:
		kinds = []permission.OperationKind{permission.OperationRead, permission.OperationWrite}
	}

	target := fileToolPermissionTarget(call)

	action := toolName + " file tool"
	if target != "" {
		action += " " + target
	}

	ops := make([]permission.Operation, 0, len(kinds))
	for _, kind := range kinds {
		ops = append(ops, permission.Operation{
			Kind:   kind,
			Action: action,
			Source: "llm." + toolName + "_tool",
			Target: target,
		})
	}

	return ops, true
}

func fileToolPermissionTarget(call ToolCall) string {
	switch strings.TrimSpace(call.Name) {
	case ToolNameGlob:
		return stringInput(call.Input, "pattern")
	case ToolNameGrep:
		target := stringInput(call.Input, "path")
		if target == "" {
			target = "."
		}

		pattern := stringInput(call.Input, "pattern")
		if pattern != "" {
			return target + " pattern=" + pattern
		}

		return target
	default:
		return stringInput(call.Input, "path")
	}
}

func stringInput(input map[string]any, key string) string {
	if input == nil {
		return ""
	}

	value, ok := input[key].(string)
	if !ok {
		return ""
	}

	return strings.TrimSpace(value)
}

func fileToolNeedsPermissionPrecheck(ctx context.Context, policy *permission.Policy, ops []permission.Operation) bool {
	if policy == nil {
		return false
	}

	for _, op := range ops {
		switch policy.ModeFor(op.Kind) {
		case permission.ModeDeny:
			return true
		case permission.ModeAsk:
			if permission.ConfirmerFromContext(ctx) == nil {
				return true
			}
		}
	}

	return false
}

func bashToolNeedsPermissionPrecheck(ctx context.Context, policy *permission.Policy, ops []permission.Operation) bool {
	if policy == nil {
		return false
	}

	for _, op := range ops {
		switch policy.ModeFor(op.Kind) {
		case permission.ModeDeny:
			if bashToolReadOnlyExecutionAllowed(policy, ops, op.Kind) {
				continue
			}

			return true
		case permission.ModeAsk:
			if permission.ConfirmerFromContext(ctx) == nil {
				return true
			}
		}
	}

	return false
}

func bashToolReadOnlyExecutionAllowed(policy *permission.Policy, ops []permission.Operation, denied permission.OperationKind) bool {
	return denied == permission.OperationExecute && permission.AllowsReadOnlyExecution(*policy, ops)
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
		if reason, rule := bashFieldsRequireConfirmation(fields); rule != "" {
			return reason, rule
		}
	}

	return "", ""
}

func bashFieldsRequireConfirmation(fields []string) (reason, rule string) {
	if len(fields) == 0 {
		return "", ""
	}

	switch strings.ToLower(fields[0]) {
	case "sudo", "su":
		return "privileged command requires confirmation", "bash.confirm.privileged"
	}

	fields = unwrapBashCommandFields(fields)
	if len(fields) == 0 {
		return "", ""
	}

	name := strings.ToLower(fields[0])

	args := fields[1:]
	if packageOrInfrastructureCommandRequiresRemoteConfirmation(name, args) {
		return "remote-mutating command requires confirmation", "bash.confirm.remote_mutation"
	}

	return bashNamedCommandRequiresConfirmation(name, args)
}

func bashNamedCommandRequiresConfirmation(name string, args []string) (reason, rule string) {
	switch name {
	case "git":
		if gitCommandRequiresConfirmation(args) {
			return "destructive git command requires confirmation", "bash.confirm.git_destructive"
		}
	case dependencyCommandGo, dependencyCommandNPM, dependencyCommandPNPM, dependencyCommandYarn, dependencyCommandPip, dependencyCommandPoetry, dependencyCommandCargo, dependencyCommandBrew, dependencyCommandUV:
		if dependencyCommandRequiresConfirmation(name, args) {
			return "dependency-changing command requires confirmation", "bash.confirm.dependency_change"
		}
	case "python", "python3":
		if pythonPipCommandMutates(name, args) {
			return "dependency-changing command requires confirmation", "bash.confirm.dependency_change"
		}
	}

	return "", ""
}

func packageOrInfrastructureCommandRequiresRemoteConfirmation(name string, args []string) bool {
	if finding, ok := classifyHTTPRemoteCommand(name, args); ok && finding.action == autonomy.ActionRemoteMutation {
		return true
	}

	if finding, ok := classifyRemoteAccessCommand(name, args); ok && finding.action == autonomy.ActionRemoteMutation {
		return true
	}

	if finding, ok := classifyPackageRemoteCommand(name, args); ok && finding.action == autonomy.ActionRemoteMutation {
		return true
	}

	if finding, ok := classifyInfrastructureCommand(name, args); ok && finding.action == autonomy.ActionRemoteMutation {
		return true
	}

	if finding, ok := classifyCloudRemoteCommand(name, args); ok && finding.action == autonomy.ActionRemoteMutation {
		return true
	}

	return false
}

func bashCommandSegments(command string) [][]string {
	parts := make([]string, 0)

	var (
		current strings.Builder
		quote   byte
		escaped bool
	)

	flush := func() {
		if part := strings.TrimSpace(current.String()); part != "" {
			parts = append(parts, part)
		}

		current.Reset()
	}

	for i := range len(command) {
		c := command[i]

		if escaped {
			current.WriteByte(c)

			escaped = false

			continue
		}

		if c == '\\' && quote != '\'' {
			current.WriteByte(c)

			escaped = true

			continue
		}

		if quote != 0 {
			current.WriteByte(c)

			if c == quote {
				quote = 0
			}

			continue
		}

		switch c {
		case '\'', '"':
			quote = c
			current.WriteByte(c)
		case ';', '&', '|', '\n', '\r', '(', ')':
			flush()
		default:
			current.WriteByte(c)
		}
	}

	flush()

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

func ddWritesOutput(args []string) bool {
	for _, arg := range args {
		arg = strings.ToLower(strings.TrimSpace(arg))
		if !strings.HasPrefix(arg, "of=") {
			continue
		}

		target := strings.TrimSpace(strings.TrimPrefix(arg, "of="))
		if target == "" || target == "/dev/null" {
			continue
		}

		return true
	}

	return false
}

func downloadCommandWritesFile(name string, args []string) bool {
	switch name {
	case commandCurl:
		return curlCommandWritesFile(args)
	case commandWget:
		return wgetCommandWritesFile(args)
	default:
		return false
	}
}

func curlCommandWritesFile(args []string) bool {
	for i, arg := range args {
		raw := strings.TrimSpace(arg)
		lower := strings.ToLower(raw)

		if lower == "-o" || lower == "--output" || lower == "--output-dir" {
			return nextArgWritesFile(args, i)
		}

		if raw == "-O" || lower == "--remote-name" {
			return true
		}

		if strings.HasPrefix(lower, "-o") && len(lower) > len("-o") {
			return strings.TrimSpace(raw[len("-o"):]) != "-"
		}

		if strings.HasPrefix(lower, "--output=") || strings.HasPrefix(lower, "--output-dir=") {
			return true
		}
	}

	return false
}

func wgetCommandWritesFile(args []string) bool {
	for i, arg := range args {
		raw := strings.TrimSpace(arg)
		lower := strings.ToLower(raw)

		if wgetOptionTakesOutputValue(raw, lower) {
			return nextArgWritesFile(args, i)
		}

		if wgetInlineOutputFlag(raw) {
			return strings.TrimSpace(raw[len("-O"):]) != "-"
		}

		if wgetOptionHasInlineOutputValue(lower) {
			return true
		}
	}

	return len(args) > 0
}

func curlCommandMutatesRemote(args []string) bool {
	method, explicitMethod := curlHTTPMethod(args)
	if httpMethodMutates(method) {
		return true
	}

	if explicitMethod {
		return strings.TrimSpace(method) != "" && !httpMethodReads(method)
	}

	return curlCommandSendsBody(args) || curlCommandUploads(args)
}

func curlCommandMergesPullRequest(args []string) bool {
	return curlCommandMutatesRemote(args) && commandArgsContainPullRequestMergeOperation(args)
}

func curlHTTPMethod(args []string) (string, bool) {
	for i := range args {
		raw := strings.TrimSpace(args[i])
		lower := strings.ToLower(raw)

		if raw == "-X" || lower == "--request" {
			if i+1 < len(args) {
				return strings.ToUpper(strings.TrimSpace(args[i+1])), true
			}

			return "", true
		}

		switch {
		case strings.HasPrefix(raw, "-X") && len(raw) > len("-X"):
			return strings.ToUpper(strings.TrimSpace(raw[len("-X"):])), true
		case strings.HasPrefix(lower, "--request="):
			return strings.ToUpper(strings.TrimSpace(raw[len("--request="):])), true
		}
	}

	return "", false
}

func curlCommandSendsBody(args []string) bool {
	for _, arg := range args {
		raw := strings.TrimSpace(arg)

		lower := strings.ToLower(raw)
		switch {
		case lower == "-d",
			lower == "--data",
			lower == "--data-raw",
			lower == "--data-binary",
			lower == "--data-urlencode",
			raw == "-F",
			lower == "--form",
			lower == "--form-string",
			lower == "--json",
			strings.HasPrefix(lower, "-d") && len(lower) > len("-d"),
			strings.HasPrefix(lower, "--data="),
			strings.HasPrefix(lower, "--data-raw="),
			strings.HasPrefix(lower, "--data-binary="),
			strings.HasPrefix(lower, "--data-urlencode="),
			strings.HasPrefix(raw, "-F") && len(raw) > len("-F"),
			strings.HasPrefix(lower, "--form="),
			strings.HasPrefix(lower, "--form-string="),
			strings.HasPrefix(lower, "--json="):
			return true
		}
	}

	return false
}

func curlCommandUploads(args []string) bool {
	for _, arg := range args {
		lower := strings.ToLower(strings.TrimSpace(arg))
		switch {
		case lower == "-t",
			lower == "--upload-file",
			strings.HasPrefix(lower, "-t") && len(lower) > len("-t"),
			strings.HasPrefix(lower, "--upload-file="):
			return true
		}
	}

	return false
}

func wgetCommandMutatesRemote(args []string) bool {
	method, explicitMethod := wgetHTTPMethod(args)
	if httpMethodMutates(method) {
		return true
	}

	if explicitMethod {
		return strings.TrimSpace(method) != "" && !httpMethodReads(method)
	}

	return wgetCommandSendsBody(args)
}

func wgetCommandMergesPullRequest(args []string) bool {
	return wgetCommandMutatesRemote(args) && commandArgsContainPullRequestMergeOperation(args)
}

func wgetHTTPMethod(args []string) (string, bool) {
	for i := range args {
		raw := strings.TrimSpace(args[i])
		lower := strings.ToLower(raw)

		if lower == "--method" {
			if i+1 < len(args) {
				return strings.ToUpper(strings.TrimSpace(args[i+1])), true
			}

			return "", true
		}

		if strings.HasPrefix(lower, "--method=") {
			return strings.ToUpper(strings.TrimSpace(raw[len("--method="):])), true
		}
	}

	return "", false
}

func wgetCommandSendsBody(args []string) bool {
	for _, arg := range args {
		lower := strings.ToLower(strings.TrimSpace(arg))
		switch {
		case lower == "--body-data",
			lower == "--body-file",
			lower == "--post-data",
			lower == "--post-file",
			strings.HasPrefix(lower, "--body-data="),
			strings.HasPrefix(lower, "--body-file="),
			strings.HasPrefix(lower, "--post-data="),
			strings.HasPrefix(lower, "--post-file="):
			return true
		}
	}

	return false
}

func httpieCommandMutatesRemote(args []string) bool {
	method, ok := firstHTTPieMethod(args)
	if !ok {
		return httpieCommandSendsBody(args)
	}

	return !httpMethodReads(method)
}

func httpieCommandMergesPullRequest(args []string) bool {
	return httpieCommandMutatesRemote(args) && commandArgsContainPullRequestMergeOperation(args)
}

func commandArgsContainPullRequestMergeOperation(args []string) bool {
	return slices.ContainsFunc(args, commandArgContainsPullRequestMergeOperation)
}

func commandArgContainsPullRequestMergeOperation(arg string) bool {
	arg = strings.ToLower(strings.TrimSpace(arg))

	return strings.Contains(arg, "mergepullrequest") ||
		strings.Contains(arg, "enablepullrequestautomerge") ||
		(strings.Contains(arg, "/pulls/") && strings.Contains(arg, "/merge"))
}

func firstHTTPieMethod(args []string) (string, bool) {
	for i := 0; i < len(args); i++ {
		arg := strings.TrimSpace(args[i])
		if arg == "" {
			continue
		}

		if arg == "--" {
			return "", false
		}

		if strings.HasPrefix(arg, "-") {
			if httpieOptionTakesValue(arg) && i+1 < len(args) {
				i++
			}

			continue
		}

		method := strings.ToUpper(arg)
		if httpieMethodToken(method) {
			return method, true
		}
	}

	return "", false
}

func httpieMethodToken(method string) bool {
	method = strings.TrimSpace(method)
	if method == "" {
		return false
	}

	for _, r := range method {
		if r < 'A' || r > 'Z' {
			return false
		}
	}

	return true
}

func httpieOptionTakesValue(arg string) bool {
	arg = strings.ToLower(strings.TrimSpace(arg))
	switch arg {
	case "--session", "--session-read-only", "--auth", "-a", "--proxy", "--verify", "--cert", "--cert-key", "--timeout":
		return true
	default:
		return false
	}
}

func httpieCommandSendsBody(args []string) bool {
	seenTarget := false

	for i := 0; i < len(args); i++ {
		arg := strings.TrimSpace(args[i])
		if arg == "" {
			continue
		}

		if strings.HasPrefix(arg, "-") {
			if httpieOptionTakesValue(arg) && i+1 < len(args) {
				i++
			}

			continue
		}

		if !seenTarget {
			seenTarget = true
			continue
		}

		if httpieRequestItemSendsBody(arg) {
			return true
		}
	}

	return false
}

func httpieRequestItemSendsBody(arg string) bool {
	arg = strings.TrimSpace(arg)
	if arg == "" || strings.HasPrefix(arg, "-") {
		return false
	}

	if strings.HasPrefix(arg, "@") {
		return true
	}

	if strings.Contains(arg, "==") {
		return false
	}

	return strings.Contains(arg, "=")
}

func httpMethodReads(method string) bool {
	switch strings.ToUpper(strings.TrimSpace(method)) {
	case "", httpMethodGet, httpMethodHead, httpMethodOptions, httpMethodTrace:
		return true
	default:
		return false
	}
}

func httpMethodMutates(method string) bool {
	switch strings.ToUpper(strings.TrimSpace(method)) {
	case httpMethodPost, httpMethodPut, httpMethodPatch, httpMethodDelete:
		return true
	default:
		return false
	}
}

func sshCommandStartsRemoteSession(args []string) bool {
	if sshCommandReadOnly(args) {
		return false
	}

	for i := 0; i < len(args); i++ {
		arg := strings.TrimSpace(args[i])
		if arg == "" {
			continue
		}

		if arg == "--" {
			return i+1 < len(args) && strings.TrimSpace(args[i+1]) != ""
		}

		if strings.HasPrefix(arg, "-") {
			if sshOptionTakesValue(arg) && i+1 < len(args) {
				i++
			}

			continue
		}

		return true
	}

	return false
}

func sshCommandReadOnly(args []string) bool {
	for _, arg := range args {
		arg = strings.TrimSpace(arg)
		switch arg {
		case "-V", "-G":
			return true
		}
	}

	return false
}

func sshOptionTakesValue(arg string) bool {
	arg = strings.TrimSpace(arg)
	switch arg {
	case "-B", "-b", "-c", "-D", "-E", "-e", "-F", "-I", "-i", "-J", "-L", "-l", "-m", "-O", "-o", "-p", "-Q", "-R", "-S", "-W", "-w":
		return true
	default:
		return false
	}
}

func wgetOptionTakesOutputValue(raw, lower string) bool {
	switch raw {
	case "-O", "-o", "-a", "-P":
		return true
	}

	switch lower {
	case "--output-document", "--output-file", "--append-output", "--directory-prefix":
		return true
	default:
		return false
	}
}

func wgetInlineOutputFlag(raw string) bool {
	return len(raw) > len("-O") &&
		(strings.HasPrefix(raw, "-O") ||
			strings.HasPrefix(raw, "-o") ||
			strings.HasPrefix(raw, "-a"))
}

func wgetOptionHasInlineOutputValue(lower string) bool {
	return strings.HasPrefix(lower, "--output-document=") ||
		strings.HasPrefix(lower, "--output-file=") ||
		strings.HasPrefix(lower, "--append-output=") ||
		strings.HasPrefix(lower, "--directory-prefix=")
}

func nextArgWritesFile(args []string, index int) bool {
	if index+1 >= len(args) {
		return false
	}

	return strings.TrimSpace(args[index+1]) != "-"
}

func perlCommandEditsInPlace(args []string) bool {
	return containsAnyExactFlag(args, "-pi", "-p -i") || containsAnyFlag(args, "i")
}

func sedCommandEditsInPlace(args []string) bool {
	for _, arg := range args {
		arg = strings.ToLower(strings.TrimSpace(arg))
		if arg == "-i" || arg == "--in-place" {
			return true
		}

		if strings.HasPrefix(arg, "-i") && len(arg) > len("-i") {
			return true
		}

		if strings.HasPrefix(arg, "--in-place=") {
			return true
		}
	}

	return false
}

func tarCommandExtracts(args []string) bool {
	for _, arg := range args {
		arg = strings.ToLower(strings.TrimSpace(arg))
		if arg == "--extract" || arg == "-x" {
			return true
		}

		if strings.HasPrefix(arg, "-") && !strings.HasPrefix(arg, "--") && strings.Contains(arg[1:], "x") {
			return true
		}
	}

	return false
}

func unzipCommandExtracts(args []string) bool {
	for _, arg := range args {
		arg = strings.ToLower(strings.TrimSpace(arg))
		if arg == "-l" || arg == "-v" || arg == "-t" || arg == "-z" {
			return false
		}
	}

	return len(args) > 0
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
	args = stripGitGlobalOptions(args)
	if len(args) == 0 {
		return false
	}

	switch strings.ToLower(args[0]) {
	case "reset":
		return containsArg(args[1:], "--hard")
	case "clean":
		return containsAnyFlag(args[1:], "f") && containsAnyFlag(args[1:], "d")
	case commandActionPush:
		return gitPushRequiresConfirmation(args[1:])
	default:
		return false
	}
}

func gitPushRequiresConfirmation(args []string) bool {
	for _, arg := range args {
		arg = strings.ToLower(strings.TrimSpace(arg))
		if arg == "" {
			continue
		}

		switch {
		case arg == "-f",
			arg == "--force",
			strings.HasPrefix(arg, "--force="),
			arg == "--force-with-lease",
			strings.HasPrefix(arg, "--force-with-lease="),
			arg == "--mirror",
			arg == gitDeleteLongFlag:
			return true
		case strings.HasPrefix(arg, "-") && !strings.HasPrefix(arg, "--") && strings.Contains(arg[1:], "f"):
			return true
		}
	}

	return false
}

func dependencyCommandRequiresConfirmation(name string, args []string) bool {
	if len(args) == 0 {
		return false
	}

	first := strings.ToLower(args[0])

	second := ""
	if len(args) > 1 {
		second = strings.ToLower(args[1])
	}

	switch name {
	case dependencyCommandGo:
		return first == "get" || first == dependencyActionInstall
	case dependencyCommandNPM, dependencyCommandPNPM:
		return dependencyActionIn(first, dependencyActionInstall, dependencyActionAdd, "ci", commandActionUpdate, "remove", "uninstall", "rm", "i")
	case dependencyCommandYarn:
		return dependencyActionIn(first, dependencyActionAdd, dependencyActionInstall, "upgrade", "remove")
	case dependencyCommandPip:
		return dependencyActionIn(first, dependencyActionInstall, "uninstall")
	case dependencyCommandPoetry:
		return dependencyActionIn(first, dependencyActionAdd, dependencyActionInstall, commandActionUpdate, "remove")
	case dependencyCommandCargo:
		return dependencyActionIn(first, dependencyActionInstall, dependencyActionAdd, commandActionUpdate)
	case dependencyCommandBrew:
		return dependencyActionIn(first, dependencyActionInstall, "uninstall", "upgrade")
	case dependencyCommandUV:
		return dependencyActionIn(first, dependencyActionAdd, "remove", "sync") ||
			(first == dependencyCommandPip && dependencyActionIn(second, dependencyActionInstall, "uninstall"))
	default:
		return false
	}
}

func pythonPipCommandMutates(name string, args []string) bool {
	if name != "python" && name != "python3" {
		return false
	}

	for len(args) > 0 && strings.HasPrefix(args[0], "-") && args[0] != "-m" {
		args = args[1:]
	}

	return len(args) >= 3 &&
		args[0] == "-m" &&
		strings.EqualFold(args[1], dependencyCommandPip) &&
		dependencyCommandRequiresConfirmation(dependencyCommandPip, args[2:])
}

func dependencyActionIn(action string, candidates ...string) bool {
	return slices.Contains(candidates, action)
}

func goModCommandMutates(name string, args []string) bool {
	if name != dependencyCommandGo || len(args) == 0 {
		return false
	}

	if !strings.EqualFold(args[0], "mod") || len(args) < 2 {
		return false
	}

	switch strings.ToLower(args[1]) {
	case gitActionEdit, "tidy", "vendor":
		return true
	default:
		return false
	}
}

func gitMergeAvoidsCommit(args []string) bool {
	return containsArg(args, "--no-commit") ||
		containsArg(args, "--squash") ||
		gitSequencerControlOnly(args)
}

func gitSequencerAvoidsCommit(args []string) bool {
	return containsArg(args, "-n") ||
		containsArg(args, "--no-commit") ||
		gitSequencerControlOnly(args)
}

func gitSequencerControlOnly(args []string) bool {
	return containsArg(args, "--abort") ||
		containsArg(args, "--quit") ||
		containsArg(args, "--skip")
}

func gitCheckoutCreatesBranch(args []string) bool {
	for _, arg := range args {
		lower := strings.ToLower(strings.TrimSpace(arg))
		switch {
		case lower == "-b",
			strings.HasPrefix(lower, "-b") && len(lower) > len("-b"),
			gitTrackBranchOption(lower),
			lower == "--guess",
			lower == "--orphan",
			strings.HasPrefix(lower, "--orphan="):
			return true
		}
	}

	return false
}

func gitSwitchCreatesBranch(args []string) bool {
	for _, arg := range args {
		lower := strings.ToLower(strings.TrimSpace(arg))
		switch {
		case lower == "-c",
			strings.HasPrefix(lower, "-c") && len(lower) > len("-c"),
			gitTrackBranchOption(lower),
			lower == "--guess",
			lower == "--create",
			lower == "--force-create",
			strings.HasPrefix(lower, "--create="),
			strings.HasPrefix(lower, "--force-create="):
			return true
		}
	}

	return false
}

func gitTrackBranchOption(lower string) bool {
	if lower == "--track" || strings.HasPrefix(lower, "--track=") {
		return true
	}

	if strings.HasPrefix(lower, "--") {
		return false
	}

	return lower == "-t" || (strings.HasPrefix(lower, "-") && strings.Contains(lower[1:], "t"))
}

func gitBranchCreates(args []string) bool {
	for _, arg := range args {
		if strings.TrimSpace(arg) == "" {
			continue
		}

		if strings.HasPrefix(arg, "-") {
			switch arg {
			case "-d", "-D", "-m", "-M", "-r", "-a", gitDeleteLongFlag, "--move", "--remotes", "--all", gitListLongFlag, "--show-current":
				return false
			}

			continue
		}

		return true
	}

	return false
}

func gitBranchMutates(args []string) bool {
	for _, arg := range args {
		lower := strings.ToLower(strings.TrimSpace(arg))
		switch {
		case lower == "-d",
			lower == "-m",
			lower == "-u",
			lower == gitDeleteLongFlag,
			lower == "--edit-description",
			lower == "--move",
			lower == "--set-upstream-to",
			lower == "--unset-upstream",
			strings.HasPrefix(lower, "--set-upstream-to="):
			return true
		}
	}

	return false
}

func gitConfigMutates(args []string) bool {
	if gitConfigHasMutationAction(args) {
		return true
	}

	if gitConfigHasReadOnlyAction(args) {
		return false
	}

	positional := gitConfigPositionalArgs(args)
	if len(positional) == 0 {
		return false
	}

	switch strings.ToLower(positional[0]) {
	case gitActionEdit, "remove-section", "rename-section", "set", "unset":
		return true
	case "get", "get-all", "get-color", "get-colorbool", "get-regexp", "get-urlmatch", "list":
		return false
	}

	// Old-style `git config <key> <value>` writes; `git config <key>` reads.
	return len(positional) >= 2
}

func gitConfigHasMutationAction(args []string) bool {
	for _, arg := range args {
		arg = strings.ToLower(strings.TrimSpace(arg))
		if arg == "" {
			continue
		}

		switch arg {
		case "-e", "--add", "--edit", "--remove-section", "--rename-section", "--replace-all", "--unset", "--unset-all":
			return true
		}

		if strings.HasPrefix(arg, "--add=") ||
			strings.HasPrefix(arg, "--replace-all=") ||
			strings.HasPrefix(arg, "--unset=") ||
			strings.HasPrefix(arg, "--unset-all=") ||
			strings.HasPrefix(arg, "--rename-section=") ||
			strings.HasPrefix(arg, "--remove-section=") {
			return true
		}
	}

	return false
}

func gitConfigHasReadOnlyAction(args []string) bool {
	for _, arg := range args {
		arg = strings.ToLower(strings.TrimSpace(arg))
		switch arg {
		case "-l", "--get", "--get-all", "--get-color", "--get-colorbool", "--get-regexp", "--get-urlmatch", gitListLongFlag:
			return true
		}
	}

	return false
}

func gitConfigPositionalArgs(args []string) []string {
	positionals := make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		arg := strings.TrimSpace(args[i])
		if arg == "" {
			continue
		}

		if arg == "--" {
			positionals = append(positionals, args[i+1:]...)
			break
		}

		if strings.HasPrefix(arg, "-") {
			if gitConfigOptionTakesValue(arg) && i+1 < len(args) {
				i++
			}

			continue
		}

		positionals = append(positionals, arg)
	}

	return positionals
}

func gitConfigOptionTakesValue(arg string) bool {
	arg = strings.ToLower(strings.TrimSpace(arg))
	switch arg {
	case "-f", "-t", "--blob", "--default", "--file", "--type":
		return true
	default:
		return false
	}
}

func firstGitSubcommandArg(args []string) string {
	return firstCommandSubcommandArg(args)
}

func firstCommandSubcommandArg(args []string) string {
	for _, arg := range args {
		arg = strings.TrimSpace(arg)
		if arg == "" {
			continue
		}

		if arg == "--" {
			continue
		}

		if strings.HasPrefix(arg, "-") {
			continue
		}

		return strings.ToLower(arg)
	}

	return ""
}

func stripPackageGlobalOptions(args []string) []string {
	return stripLeadingOptions(args, map[string]bool{
		"-C":               true,
		"--cache":          true,
		"--cwd":            true,
		"--registry":       true,
		"--scope":          true,
		"--userconfig":     true,
		"-w":               true,
		"--workspace":      true,
		"--workspace-root": true,
	}, []string{
		"--cache=",
		"--cwd=",
		"--registry=",
		"--scope=",
		"--userconfig=",
		"--workspace=",
		"--workspace-root=",
		"-C",
		"-w",
	})
}

func stripDockerGlobalOptions(args []string) []string {
	return stripLeadingOptions(args, map[string]bool{
		"-c":        true,
		"--config":  true,
		"--context": true,
		"-H":        true,
		"--host":    true,
	}, []string{
		"--config=",
		"--context=",
		"--host=",
		"-H",
	})
}

func stripDockerComposeGlobalOptions(args []string) []string {
	return stripLeadingOptions(args, map[string]bool{
		"--env-file":          true,
		"-f":                  true,
		"--file":              true,
		"--profile":           true,
		"--project-name":      true,
		"-p":                  true,
		"--project-directory": true,
	}, []string{
		"--env-file=",
		"--file=",
		"--profile=",
		"--project-name=",
		"--project-directory=",
		"-f",
		"-p",
	})
}

func stripKubectlGlobalOptions(args []string) []string {
	return stripLeadingOptions(args, map[string]bool{
		"--as":                    true,
		"--as-group":              true,
		"--cache-dir":             true,
		"--certificate-authority": true,
		"--client-certificate":    true,
		"--client-key":            true,
		"--cluster":               true,
		"--context":               true,
		"--field-manager":         true,
		"--kubeconfig":            true,
		"-n":                      true,
		"--namespace":             true,
		"--password":              true,
		"--profile":               true,
		"--profile-output":        true,
		"--request-timeout":       true,
		"-s":                      true,
		"--server":                true,
		"--token":                 true,
		"--user":                  true,
		"--username":              true,
	}, []string{
		"--as=",
		"--as-group=",
		"--cache-dir=",
		"--certificate-authority=",
		"--client-certificate=",
		"--client-key=",
		"--cluster=",
		"--context=",
		"--field-manager=",
		"--kubeconfig=",
		"--namespace=",
		"--password=",
		"--profile=",
		"--profile-output=",
		"--request-timeout=",
		"--server=",
		"--token=",
		"--user=",
		"--username=",
		"-n",
		"-s",
	})
}

func stripHelmGlobalOptions(args []string) []string {
	return stripLeadingOptions(args, map[string]bool{
		"--burst-limit":       true,
		"--kube-apiserver":    true,
		"--kube-as-group":     true,
		"--kube-as-user":      true,
		"--kube-ca-file":      true,
		"--kube-context":      true,
		"--kube-token":        true,
		"--kubeconfig":        true,
		"-n":                  true,
		"--namespace":         true,
		"--registry-config":   true,
		"--repository-cache":  true,
		"--repository-config": true,
	}, []string{
		"--burst-limit=",
		"--kube-apiserver=",
		"--kube-as-group=",
		"--kube-as-user=",
		"--kube-ca-file=",
		"--kube-context=",
		"--kube-token=",
		"--kubeconfig=",
		"--namespace=",
		"--registry-config=",
		"--repository-cache=",
		"--repository-config=",
		"-n",
	})
}

func stripTerraformGlobalOptions(args []string) []string {
	return stripLeadingOptions(args, map[string]bool{
		"-chdir": true,
	}, []string{
		"-chdir=",
	})
}

func stripPulumiGlobalOptions(args []string) []string {
	return stripLeadingOptions(args, map[string]bool{
		"-C":        true,
		"--cwd":     true,
		"--stack":   true,
		"-s":        true,
		"--color":   true,
		"--tracing": true,
	}, []string{
		"--cwd=",
		"--stack=",
		"--color=",
		"--tracing=",
		"-C",
		"-s",
	})
}

func stripAWSGlobalOptions(args []string) []string {
	return stripLeadingOptions(args, map[string]bool{
		"--ca-bundle":      true,
		"--cli-input-json": true,
		"--cli-input-yaml": true,
		"--color":          true,
		"--endpoint-url":   true,
		"--output":         true,
		"--profile":        true,
		"--query":          true,
		"--region":         true,
	}, []string{
		"--ca-bundle=",
		"--cli-input-json=",
		"--cli-input-yaml=",
		"--color=",
		"--endpoint-url=",
		"--output=",
		"--profile=",
		"--query=",
		"--region=",
	})
}

func stripGCloudGlobalOptions(args []string) []string {
	return stripLeadingOptions(args, map[string]bool{
		"--account":                     true,
		"--billing-project":             true,
		"--configuration":               true,
		"--flags-file":                  true,
		"--format":                      true,
		"--impersonate-service-account": true,
		"--project":                     true,
		"--quiet":                       false,
	}, []string{
		"--account=",
		"--billing-project=",
		"--configuration=",
		"--flags-file=",
		"--format=",
		"--impersonate-service-account=",
		"--project=",
	})
}

func stripAzureGlobalOptions(args []string) []string {
	return stripLeadingOptions(args, map[string]bool{
		"--output":       true,
		"--query":        true,
		"--subscription": true,
	}, []string{
		"--output=",
		"--query=",
		"--subscription=",
	})
}

func stripGenericCloudGlobalOptions(args []string) []string {
	return stripLeadingOptions(args, map[string]bool{
		"--cwd":     true,
		"--project": true,
		"--scope":   true,
		"--team":    true,
		"--token":   true,
	}, []string{
		"--cwd=",
		"--project=",
		"--scope=",
		"--team=",
		"--token=",
	})
}

func gitReplaceMutates(args []string) bool {
	listMode := false

	for i := 0; i < len(args); i++ {
		arg := strings.ToLower(strings.TrimSpace(args[i]))
		if arg == "" {
			continue
		}

		switch {
		case arg == "-l" || arg == gitListLongFlag || strings.HasPrefix(arg, "--format="):
			listMode = true
			continue
		case arg == "--format":
			listMode = true

			if i+1 < len(args) {
				i++
			}

			continue
		case listMode:
			continue
		default:
			return true
		}
	}

	return false
}

func gitSymbolicRefMutates(args []string) bool {
	positionals := make([]string, 0, len(args))
	for _, arg := range args {
		arg = strings.TrimSpace(arg)
		if arg == "" {
			continue
		}

		if arg == "--" {
			continue
		}

		if strings.EqualFold(arg, "-d") || strings.EqualFold(arg, "--delete") {
			return true
		}

		if strings.HasPrefix(arg, "-") {
			continue
		}

		positionals = append(positionals, arg)
	}

	return len(positionals) >= 2
}

func gitUpdateRefUsesStdin(args []string) bool {
	return containsArg(args, "--stdin")
}

func gitUpdateRefTarget(args []string) (ref string, deleting bool) {
	for i := 0; i < len(args); i++ {
		arg := strings.TrimSpace(args[i])
		lower := strings.ToLower(arg)

		if arg == "" {
			continue
		}

		if arg == "--" {
			for _, positional := range args[i+1:] {
				if strings.TrimSpace(positional) != "" {
					return strings.TrimSpace(positional), deleting
				}
			}

			return "", deleting
		}

		switch {
		case lower == "-d" || lower == gitDeleteLongFlag:
			deleting = true

			continue
		case lower == "-m":
			if i+1 < len(args) {
				i++
			}

			continue
		case strings.HasPrefix(lower, "-m") && len(lower) > len("-m"):
			continue
		case strings.HasPrefix(arg, "-"):
			continue
		default:
			return arg, deleting
		}
	}

	return "", deleting
}

func gitRefIsBranch(ref string) bool {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return false
	}

	return strings.HasPrefix(ref, "refs/heads/") || strings.HasPrefix(ref, "heads/")
}

func shellCommandArg(args []string) (string, bool) {
	for i, arg := range args {
		arg = strings.TrimSpace(arg)
		if arg == "" {
			continue
		}

		if shellOptionRunsCommand(arg) {
			if i+1 >= len(args) {
				return "", false
			}

			command := trimShellCommandQuotes(strings.Join(args[i+1:], " "))

			return command, strings.TrimSpace(command) != ""
		}
	}

	return "", false
}

func shellInterpreterRunsScript(args []string) bool {
	if shellInterpreterNoExec(args) {
		return false
	}

	for i := 0; i < len(args); i++ {
		arg := strings.TrimSpace(args[i])
		if arg == "" {
			continue
		}

		if arg == "--" {
			return i+1 < len(args) && strings.TrimSpace(args[i+1]) != ""
		}

		if arg == "-" || arg == "-s" || (strings.HasPrefix(arg, "-") && !strings.HasPrefix(arg, "--") && strings.Contains(arg[1:], "s")) {
			return true
		}

		if strings.HasPrefix(arg, "-") {
			if shellInterpreterOptionTakesValue(arg) && i+1 < len(args) {
				i++
			}

			continue
		}

		return true
	}

	return false
}

func shellInterpreterNoExec(args []string) bool {
	for _, arg := range args {
		arg = strings.TrimSpace(arg)
		if arg == "" {
			continue
		}

		if arg == "--" {
			return false
		}

		if arg == "-n" || arg == "--noexec" || (strings.HasPrefix(arg, "-") && !strings.HasPrefix(arg, "--") && strings.Contains(arg[1:], "n")) {
			return true
		}

		if !strings.HasPrefix(arg, "-") {
			return false
		}
	}

	return false
}

func shellInterpreterOptionTakesValue(arg string) bool {
	arg = strings.ToLower(strings.TrimSpace(arg))
	switch arg {
	case "-o", "-O", "--rcfile", "--init-file":
		return true
	default:
		return false
	}
}

func shellOptionRunsCommand(arg string) bool {
	if arg == "-c" {
		return true
	}

	if strings.HasPrefix(arg, "--") {
		return false
	}

	return strings.HasPrefix(arg, "-") && strings.Contains(arg[1:], "c")
}

func trimShellCommandQuotes(command string) string {
	command = strings.TrimSpace(command)
	for len(command) >= 2 {
		first, last := command[0], command[len(command)-1]
		if (first != '"' && first != '\'') || first != last {
			break
		}

		command = strings.TrimSpace(command[1 : len(command)-1])
	}

	return command
}

func stripGitGlobalOptions(args []string) []string {
	return stripLeadingOptions(args, map[string]bool{
		"-C":             true,
		"-c":             true,
		"--git-dir":      true,
		"--work-tree":    true,
		"--namespace":    true,
		"--config-env":   true,
		"--exec-path":    true,
		"--super-prefix": true,
	}, []string{
		"-C",
		"-c",
		"--git-dir=",
		"--work-tree=",
		"--namespace=",
		"--config-env=",
		"--exec-path=",
		"--super-prefix=",
	})
}

func stripGitHubGlobalOptions(args []string) []string {
	return stripLeadingOptions(args, map[string]bool{
		"-R":           true,
		"--repo":       true,
		"--hostname":   true,
		"--config-dir": true,
	}, []string{
		"--repo=",
		"--hostname=",
		"--config-dir=",
	})
}

func stripGitNotesGlobalOptions(args []string) []string {
	return stripLeadingOptions(args, map[string]bool{
		"--notes-ref": true,
		"--ref":       true,
	}, []string{
		"--notes-ref=",
		"--ref=",
	})
}

func stripLeadingOptions(args []string, valueOptions map[string]bool, inlineValuePrefixes []string) []string {
	for len(args) > 0 {
		arg := strings.TrimSpace(args[0])
		if arg == "" {
			args = args[1:]
			continue
		}

		if arg == "--" {
			return args[1:]
		}

		if !strings.HasPrefix(arg, "-") {
			return args
		}

		if optionHasInlineValue(arg, inlineValuePrefixes) {
			args = args[1:]
			continue
		}

		if valueOptions[arg] {
			if len(args) < 2 {
				return nil
			}

			args = args[2:]

			continue
		}

		args = args[1:]
	}

	return args
}

func optionHasInlineValue(arg string, prefixes []string) bool {
	for _, prefix := range prefixes {
		if strings.HasPrefix(arg, prefix) && len(arg) > len(prefix) {
			return true
		}
	}

	return false
}

func commandArgsContainPost(args []string) bool {
	return strings.EqualFold(githubAPIMethod(args), httpMethodPost)
}

func githubAPIArgsMergePullRequest(args []string) bool {
	joined := strings.ToLower(strings.Join(args, " "))

	return strings.Contains(joined, "mergepullrequest") ||
		strings.Contains(joined, "enablepullrequestautomerge") ||
		(strings.Contains(joined, "pulls") && strings.Contains(joined, gitActionMerge))
}

func githubAPIArgsCreatePullRequest(args []string) bool {
	joined := strings.ToLower(strings.Join(args, " "))

	return strings.Contains(joined, "createpullrequest") ||
		(strings.Contains(joined, "pulls") && commandArgsContainPost(args))
}

func githubAPIArgsOpaqueGraphQLMutation(args []string) bool {
	return githubAPIArgsTargetGraphQL(args) &&
		githubAPIMethodMutates(args) &&
		!githubAPIArgsHaveInlineGraphQLQuery(args)
}

func githubAPIArgsTargetGraphQL(args []string) bool {
	for _, arg := range args {
		normalized := strings.ToLower(strings.TrimSpace(arg))
		if normalized == "graphql" || strings.HasSuffix(normalized, "/graphql") {
			return true
		}
	}

	return false
}

func githubAPIArgsHaveInlineGraphQLQuery(args []string) bool {
	for i := range args {
		raw := strings.TrimSpace(args[i])
		lower := strings.ToLower(raw)

		switch {
		case lower == "-f" || lower == "--field" || lower == "--raw-field":
			if i+1 < len(args) && githubAPIFieldIsInlineGraphQLQuery(args[i+1]) {
				return true
			}
		case strings.HasPrefix(lower, "-f") && len(raw) > len("-f"):
			if githubAPIFieldIsInlineGraphQLQuery(raw[len("-f"):]) {
				return true
			}
		case strings.HasPrefix(lower, "--field="):
			if githubAPIFieldIsInlineGraphQLQuery(raw[len("--field="):]) {
				return true
			}
		case strings.HasPrefix(lower, "--raw-field="):
			if githubAPIFieldIsInlineGraphQLQuery(raw[len("--raw-field="):]) {
				return true
			}
		}
	}

	return false
}

func githubAPIFieldIsInlineGraphQLQuery(field string) bool {
	key, value, ok := strings.Cut(strings.TrimSpace(field), "=")
	if !ok || strings.ToLower(strings.TrimSpace(key)) != "query" {
		return false
	}

	value = strings.TrimSpace(value)

	return value != "" && !strings.HasPrefix(value, "@")
}

func githubAPIMethodMutates(args []string) bool {
	if method, explicit := githubAPIExplicitMethod(args); explicit {
		method = strings.TrimSpace(method)

		return method != "" && !githubAPIReadMethod(method)
	}

	switch strings.ToUpper(githubAPIMethod(args)) {
	case httpMethodPost, httpMethodPatch, httpMethodPut, httpMethodDelete:
		return true
	default:
		return false
	}
}

func githubAPIReadMethod(method string) bool {
	switch strings.ToUpper(strings.TrimSpace(method)) {
	case httpMethodGet, httpMethodHead, httpMethodOptions:
		return true
	default:
		return false
	}
}

func githubAPIMethod(args []string) string {
	if method, ok := githubAPIExplicitMethod(args); ok {
		return method
	}

	if githubAPIArgsSendBody(args) {
		return httpMethodPost
	}

	return httpMethodGet
}

func githubAPIExplicitMethod(args []string) (string, bool) {
	for i, arg := range args {
		if strings.EqualFold(arg, "-X") || strings.EqualFold(arg, "--method") {
			if i+1 < len(args) {
				return strings.TrimSpace(args[i+1]), true
			}

			return "", true
		}

		upper := strings.ToUpper(strings.TrimSpace(arg))
		switch {
		case strings.HasPrefix(upper, "-X") && len(upper) > len("-X"):
			return strings.TrimSpace(arg[len("-X"):]), true
		case strings.HasPrefix(upper, "--METHOD="):
			return strings.TrimSpace(arg[len("--method="):]), true
		}
	}

	return "", false
}

func githubAPIArgsHaveFields(args []string) bool {
	for _, arg := range args {
		lower := strings.ToLower(strings.TrimSpace(arg))
		switch {
		case lower == "-f",
			lower == "--field",
			lower == "--raw-field",
			strings.HasPrefix(lower, "-f") && len(lower) > len("-f"),
			strings.HasPrefix(lower, "--field="),
			strings.HasPrefix(lower, "--raw-field="):
			return true
		}
	}

	return false
}

func githubAPIArgsSendBody(args []string) bool {
	if githubAPIArgsHaveFields(args) {
		return true
	}

	for _, arg := range args {
		lower := strings.ToLower(strings.TrimSpace(arg))
		switch {
		case lower == "--input",
			strings.HasPrefix(lower, "--input="):
			return true
		}
	}

	return false
}

func containsArg(args []string, want string) bool {
	for _, arg := range args {
		if strings.EqualFold(arg, want) {
			return true
		}
	}

	return false
}

func containsAnyExactFlag(args []string, flags ...string) bool {
	for _, arg := range args {
		for _, flag := range flags {
			if strings.EqualFold(arg, flag) {
				return true
			}
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
