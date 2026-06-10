package events

import (
	"fmt"
	"strings"

	"github.com/tommoulard/atteler/pkg/autonomy"
)

const (
	httpMethodDelete = "DELETE"
	httpMethodPatch  = "PATCH"
	httpMethodPost   = "POST"
	httpMethodPut    = "PUT"

	eventHookArgNamespace       = "--namespace"
	eventHookSubcommandCreate   = "create"
	eventHookSubcommandDelete   = "delete"
	eventHookSubcommandPublish  = "publish"
	eventHookSubcommandPush     = "push"
	eventHookSubcommandRollback = "rollback"
	eventHookCommandGem         = "gem"
)

func authorizeEventHookAutonomy(level autonomy.Level, command []string) error {
	level = autonomy.Normalize(level)

	action, detail := eventHookAutonomyAction(command)
	if action == "" || level.Allows(action) {
		return nil
	}

	return fmt.Errorf("events: %s", autonomy.DenialMessage(level, action, detail))
}

func eventHookAutonomyAction(command []string) (action autonomy.Action, detail string) {
	if len(command) == 0 {
		return "", ""
	}

	return eventHookAutonomyActionFields(command)
}

func eventHookAutonomyActionFields(fields []string) (action autonomy.Action, detail string) {
	fields = unwrapEventHookCommandFields(fields)
	if len(fields) == 0 {
		return "", ""
	}

	name := strings.ToLower(strings.TrimSpace(fields[0]))
	args := fields[1:]

	switch name {
	case "git":
		return eventHookGitAction(stripEventHookGitGlobalOptions(args))
	case "gh":
		return eventHookGitHubAction(stripEventHookGitHubGlobalOptions(args))
	case "curl", "wget", "http", "https":
		if eventHookHTTPMergesPullRequest(name, args) {
			return autonomy.ActionPullRequestMerge, name + " pull request merge"
		}

		if eventHookHTTPMutatesRemote(name, args) {
			return autonomy.ActionRemoteMutation, name + " remote request"
		}
	}

	if action, detail := eventHookPackageRemoteAction(name, args); action != "" {
		return action, detail
	}

	if action, detail := eventHookInfrastructureRemoteAction(name, args); action != "" {
		return action, detail
	}

	if name == "ssh" && eventHookSSHStartsRemoteSession(args) {
		return autonomy.ActionRemoteMutation, "ssh remote session"
	}

	return "", ""
}

func unwrapEventHookCommandFields(fields []string) []string {
	for len(fields) > 0 {
		name := strings.ToLower(strings.TrimSpace(fields[0]))
		switch name {
		case "env":
			fields = unwrapEventHookEnvFields(fields[1:])
		case "command", "sudo":
			fields = fields[1:]
		case "bash", "sh", "zsh":
			if script, ok := eventHookShellCommandArg(fields[1:]); ok {
				fields = eventHookScriptActionFields(script)
			} else {
				return fields
			}
		default:
			return fields
		}
	}

	return nil
}

func unwrapEventHookEnvFields(args []string) []string {
	for len(args) > 0 {
		arg := strings.TrimSpace(args[0])
		switch {
		case arg == "":
			args = args[1:]
		case strings.Contains(arg, "=") && !strings.HasPrefix(arg, "-"):
			args = args[1:]
		case arg == "--":
			return args[1:]
		case strings.HasPrefix(arg, "-"):
			args = args[1:]
		default:
			return args
		}
	}

	return nil
}

func eventHookShellCommandArg(args []string) (string, bool) {
	for i := range args {
		arg := strings.TrimSpace(args[i])
		if arg == "" {
			continue
		}

		if arg == "-c" || arg == "-lc" || arg == "-l" {
			if i+1 < len(args) {
				return args[i+1], true
			}

			return "", false
		}

		if strings.HasPrefix(arg, "-") {
			continue
		}

		return arg, true
	}

	return "", false
}

func eventHookScriptActionFields(script string) []string {
	fallback := strings.Fields(script)
	for _, segment := range strings.FieldsFunc(script, eventHookScriptSegmentSeparator) {
		fields := strings.Fields(segment)
		if len(fields) == 0 {
			continue
		}

		if action, _ := eventHookAutonomyActionFields(fields); action != "" {
			return fields
		}
	}

	return fallback
}

func eventHookScriptSegmentSeparator(r rune) bool {
	switch r {
	case '\n', ';', '&', '|':
		return true
	default:
		return false
	}
}

func stripEventHookGitGlobalOptions(args []string) []string {
	for len(args) > 0 {
		arg := strings.TrimSpace(args[0])
		if arg == "" {
			args = args[1:]
			continue
		}

		switch arg {
		case "-C", "-c", "--git-dir", "--work-tree", eventHookArgNamespace:
			if len(args) > 1 {
				args = args[2:]
				continue
			}

			return nil
		case "--":
			return args[1:]
		}

		if strings.HasPrefix(arg, "--git-dir=") ||
			strings.HasPrefix(arg, "--work-tree=") ||
			strings.HasPrefix(arg, "--namespace=") {
			args = args[1:]
			continue
		}

		if strings.HasPrefix(arg, "-") {
			args = args[1:]
			continue
		}

		return args
	}

	return nil
}

func stripEventHookGitHubGlobalOptions(args []string) []string {
	for len(args) > 0 {
		arg := strings.TrimSpace(args[0])
		if arg == "" {
			args = args[1:]
			continue
		}

		switch arg {
		case "-R", "--repo", "--hostname", "--config-dir":
			if len(args) > 1 {
				args = args[2:]
				continue
			}

			return nil
		case "--":
			return args[1:]
		}

		if strings.HasPrefix(arg, "--repo=") ||
			strings.HasPrefix(arg, "--hostname=") ||
			strings.HasPrefix(arg, "--config-dir=") {
			args = args[1:]
			continue
		}

		if strings.HasPrefix(arg, "-") {
			args = args[1:]
			continue
		}

		return args
	}

	return nil
}

func eventHookGitAction(args []string) (action autonomy.Action, detail string) {
	if len(args) == 0 {
		return "", ""
	}

	subcommand := strings.ToLower(strings.TrimSpace(args[0]))
	if action, detail := eventHookGitPublishAction(subcommand, args[1:]); action != "" {
		return action, detail
	}

	if action, detail := eventHookGitBranchAction(subcommand, args[1:]); action != "" {
		return action, detail
	}

	return "", ""
}

func eventHookGitPublishAction(subcommand string, args []string) (action autonomy.Action, detail string) {
	switch subcommand {
	case eventHookSubcommandPush:
		return autonomy.ActionPush, "git push"
	case "commit", "commit-tree":
		return autonomy.ActionCommit, "git " + subcommand
	case "merge":
		if !containsEventHookArg(args, "--no-commit") {
			return autonomy.ActionCommit, "git merge"
		}
	case "cherry-pick", "rebase", "revert":
		return autonomy.ActionCommit, "git " + subcommand
	default:
		return "", ""
	}

	return "", ""
}

func eventHookGitBranchAction(subcommand string, args []string) (action autonomy.Action, detail string) {
	switch subcommand {
	case "checkout":
		if eventHookGitCheckoutCreatesBranch(args) {
			return autonomy.ActionBranch, "git checkout branch creation"
		}
	case "switch":
		if eventHookGitSwitchCreatesBranch(args) {
			return autonomy.ActionBranch, "git switch --create"
		}
	case "branch":
		if eventHookGitBranchCreates(args) {
			return autonomy.ActionBranch, "git branch"
		}
	case "worktree":
		if len(args) > 0 && strings.EqualFold(strings.TrimSpace(args[0]), "add") {
			return autonomy.ActionBranch, "git worktree add"
		}
	}

	return "", ""
}

func eventHookGitCheckoutCreatesBranch(args []string) bool {
	for _, arg := range args {
		lower := strings.ToLower(strings.TrimSpace(arg))
		switch {
		case lower == "-b",
			strings.HasPrefix(lower, "-b") && len(lower) > len("-b"),
			lower == "--orphan",
			strings.HasPrefix(lower, "--orphan="),
			eventHookGitTrackBranchOption(lower):
			return true
		}
	}

	return false
}

func eventHookGitSwitchCreatesBranch(args []string) bool {
	for _, arg := range args {
		lower := strings.ToLower(strings.TrimSpace(arg))
		switch {
		case lower == "-c",
			strings.HasPrefix(lower, "-c") && len(lower) > len("-c"),
			lower == "--create",
			strings.HasPrefix(lower, "--create="),
			lower == "--force-create",
			strings.HasPrefix(lower, "--force-create="),
			eventHookGitTrackBranchOption(lower):
			return true
		}
	}

	return false
}

func eventHookGitTrackBranchOption(lower string) bool {
	if lower == "--track" || strings.HasPrefix(lower, "--track=") {
		return true
	}

	if strings.HasPrefix(lower, "--") {
		return false
	}

	return lower == "-t" || (strings.HasPrefix(lower, "-") && strings.Contains(lower[1:], "t"))
}

func eventHookGitBranchCreates(args []string) bool {
	for _, arg := range args {
		arg = strings.TrimSpace(arg)
		if arg == "" {
			continue
		}

		if strings.HasPrefix(arg, "-") {
			switch strings.ToLower(arg) {
			case "-d", "-D", "-m", "-M", "-r", "-a", "--delete", "--move", "--remotes", "--all", "--list", "--show-current":
				return false
			}

			continue
		}

		return true
	}

	return false
}

func containsEventHookArg(args []string, target string) bool {
	for _, arg := range args {
		if strings.EqualFold(strings.TrimSpace(arg), target) {
			return true
		}
	}

	return false
}

func eventHookGitHubAction(args []string) (action autonomy.Action, detail string) {
	if len(args) == 0 {
		return "", ""
	}

	scope := strings.ToLower(strings.TrimSpace(args[0]))
	if scope == "api" {
		if eventHookGitHubAPIMergesPullRequest(args[1:]) {
			return autonomy.ActionPullRequestMerge, "gh api pull request merge"
		}

		if eventHookGitHubAPICreatesPullRequest(args[1:]) {
			return autonomy.ActionPullRequestCreate, "gh api pull request create"
		}

		if eventHookGitHubAPIOpaqueGraphQLMutation(args[1:]) {
			return autonomy.ActionPullRequestMerge, "gh api graphql opaque mutation"
		}

		if eventHookGitHubAPIMutates(args[1:]) {
			return autonomy.ActionRemoteMutation, "gh api " + eventHookGitHubAPIMethod(args[1:])
		}

		return "", ""
	}

	if len(args) < 2 {
		return "", ""
	}

	subcommand := strings.ToLower(strings.TrimSpace(args[1]))
	if scope == "pr" {
		switch subcommand {
		case "merge":
			return autonomy.ActionPullRequestMerge, "gh pr merge"
		case eventHookSubcommandCreate:
			return autonomy.ActionPullRequestCreate, "gh pr create"
		case "checkout":
			return autonomy.ActionBranch, "gh pr checkout"
		}
	}

	if eventHookGitHubScopeMutatesRemote(scope, subcommand) {
		return autonomy.ActionRemoteMutation, "gh " + scope + " " + subcommand
	}

	return "", ""
}

func eventHookGitHubScopeMutatesRemote(scope, subcommand string) bool {
	readOnly := map[string]map[string]struct{}{
		"alias":    {"list": {}},
		"auth":     {"status": {}},
		"cache":    {"list": {}},
		"config":   {"get": {}, "list": {}},
		"gist":     {"list": {}, "view": {}},
		"gpg-key":  {"list": {}},
		"issue":    {"list": {}, "status": {}, "view": {}},
		"label":    {"list": {}},
		"pr":       {"checks": {}, "diff": {}, "list": {}, "status": {}, "view": {}},
		"project":  {"field-list": {}, "item-list": {}, "list": {}, "view": {}},
		"release":  {"list": {}, "view": {}},
		"repo":     {"list": {}, "view": {}},
		"run":      {"list": {}, "view": {}, "watch": {}},
		"secret":   {"list": {}},
		"ssh-key":  {"list": {}},
		"variable": {"list": {}},
		"workflow": {"list": {}, "view": {}},
	}

	commands, ok := readOnly[scope]
	if !ok {
		return false
	}

	_, isReadOnly := commands[subcommand]

	return !isReadOnly
}

func eventHookGitHubAPIMutates(args []string) bool {
	method, explicit := eventHookGitHubAPIExplicitMethod(args)
	if explicit {
		return strings.TrimSpace(method) != "" && !eventHookHTTPMethodReads(method)
	}

	method = eventHookGitHubAPIMethod(args)
	switch method {
	case httpMethodPost, httpMethodPut, httpMethodPatch, httpMethodDelete:
		return true
	default:
		return false
	}
}

func eventHookGitHubAPIMethod(args []string) string {
	if method, explicit := eventHookGitHubAPIExplicitMethod(args); explicit {
		return method
	}

	if eventHookGitHubAPIArgsSendBody(args) {
		return httpMethodPost
	}

	return "GET"
}

func eventHookGitHubAPIExplicitMethod(args []string) (string, bool) {
	for i := range args {
		arg := strings.TrimSpace(args[i])
		if strings.EqualFold(arg, "--method") || arg == "-X" {
			if i+1 < len(args) {
				return strings.ToUpper(strings.TrimSpace(args[i+1])), true
			}

			return "", true
		}

		if strings.HasPrefix(strings.ToLower(arg), "--method=") {
			return strings.ToUpper(strings.TrimSpace(strings.TrimPrefix(arg, "--method="))), true
		}
	}

	return "", false
}

func eventHookGitHubAPIArgsHaveFields(args []string) bool {
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

func eventHookGitHubAPIArgsSendBody(args []string) bool {
	if eventHookGitHubAPIArgsHaveFields(args) {
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

func eventHookGitHubAPIMergesPullRequest(args []string) bool {
	joined := strings.ToLower(strings.Join(args, " "))

	return eventHookGitHubAPIMutates(args) &&
		(strings.Contains(joined, "mergepullrequest") ||
			strings.Contains(joined, "enablepullrequestautomerge") ||
			(strings.Contains(joined, "/pulls/") && strings.Contains(joined, "/merge")))
}

func eventHookGitHubAPICreatesPullRequest(args []string) bool {
	joined := strings.ToLower(strings.Join(args, " "))

	return eventHookGitHubAPIMutates(args) &&
		(strings.Contains(joined, "createpullrequest") || strings.Contains(joined, "/pulls"))
}

func eventHookGitHubAPIOpaqueGraphQLMutation(args []string) bool {
	return eventHookGitHubAPITargetsGraphQL(args) &&
		eventHookGitHubAPIMutates(args) &&
		!eventHookGitHubAPIArgsHaveInlineGraphQLQuery(args)
}

func eventHookGitHubAPITargetsGraphQL(args []string) bool {
	for _, arg := range args {
		normalized := strings.ToLower(strings.TrimSpace(arg))
		if normalized == "graphql" || strings.HasSuffix(normalized, "/graphql") {
			return true
		}
	}

	return false
}

func eventHookGitHubAPIArgsHaveInlineGraphQLQuery(args []string) bool {
	for i := range args {
		raw := strings.TrimSpace(args[i])
		lower := strings.ToLower(raw)

		switch {
		case lower == "-f" || lower == "--field" || lower == "--raw-field":
			if i+1 < len(args) && eventHookGitHubAPIFieldIsInlineGraphQLQuery(args[i+1]) {
				return true
			}
		case strings.HasPrefix(lower, "-f") && len(raw) > len("-f"):
			if eventHookGitHubAPIFieldIsInlineGraphQLQuery(raw[len("-f"):]) {
				return true
			}
		case strings.HasPrefix(lower, "--field="):
			if eventHookGitHubAPIFieldIsInlineGraphQLQuery(raw[len("--field="):]) {
				return true
			}
		case strings.HasPrefix(lower, "--raw-field="):
			if eventHookGitHubAPIFieldIsInlineGraphQLQuery(raw[len("--raw-field="):]) {
				return true
			}
		}
	}

	return false
}

func eventHookGitHubAPIFieldIsInlineGraphQLQuery(field string) bool {
	key, value, ok := strings.Cut(strings.TrimSpace(field), "=")
	if !ok || strings.ToLower(strings.TrimSpace(key)) != "query" {
		return false
	}

	value = strings.TrimSpace(value)

	return value != "" && !strings.HasPrefix(value, "@")
}

func eventHookPackageRemoteAction(name string, args []string) (action autonomy.Action, detail string) {
	switch name {
	case "npm", "pnpm":
		return eventHookNPMLikeRemoteAction(name, args)
	case "yarn":
		return eventHookYarnRemoteAction(args)
	case "cargo":
		return eventHookPublishOrYankRemoteAction("cargo", args)
	case "poetry":
		return eventHookSingleRemoteSubcommandAction("poetry", args, eventHookSubcommandPublish)
	case "twine":
		return eventHookSingleRemoteSubcommandAction("twine", args, "upload")
	case eventHookCommandGem:
		return eventHookPublishOrYankRemoteAction(eventHookCommandGem, args)
	}

	return "", ""
}

func eventHookNPMLikeRemoteAction(name string, args []string) (action autonomy.Action, detail string) {
	subcommand := firstEventHookSubcommand(stripEventHookPackageGlobalOptions(args))
	switch subcommand {
	case eventHookSubcommandPublish, "unpublish", "deprecate":
		return autonomy.ActionRemoteMutation, name + " " + subcommand
	case "dist-tag", "owner", "team", "token":
		if eventHookSubcommandMutates(stripEventHookPackageGlobalOptions(args[1:])) {
			return autonomy.ActionRemoteMutation, name + " " + subcommand
		}
	}

	return "", ""
}

func eventHookYarnRemoteAction(args []string) (action autonomy.Action, detail string) {
	packageArgs := stripEventHookPackageGlobalOptions(args)
	subcommand := firstEventHookSubcommand(packageArgs)

	switch subcommand {
	case eventHookSubcommandPublish:
		return autonomy.ActionRemoteMutation, "yarn publish"
	case "npm":
		if len(packageArgs) > 1 && firstEventHookSubcommand(packageArgs[1:]) == eventHookSubcommandPublish {
			return autonomy.ActionRemoteMutation, "yarn npm publish"
		}
	}

	return "", ""
}

func eventHookPublishOrYankRemoteAction(name string, args []string) (action autonomy.Action, detail string) {
	subcommand := firstEventHookSubcommand(stripEventHookPackageGlobalOptions(args))
	switch {
	case subcommand == eventHookSubcommandPublish && name != eventHookCommandGem:
		return autonomy.ActionRemoteMutation, name + " " + subcommand
	case subcommand == eventHookSubcommandPush && name == eventHookCommandGem:
		return autonomy.ActionRemoteMutation, name + " " + subcommand
	case subcommand == "yank":
		return autonomy.ActionRemoteMutation, name + " " + subcommand
	default:
		return "", ""
	}
}

func eventHookSingleRemoteSubcommandAction(name string, args []string, target string) (action autonomy.Action, detail string) {
	if firstEventHookSubcommand(stripEventHookPackageGlobalOptions(args)) != target {
		return "", ""
	}

	return autonomy.ActionRemoteMutation, name + " " + target
}

func eventHookInfrastructureRemoteAction(name string, args []string) (action autonomy.Action, detail string) {
	switch name {
	case "docker":
		return eventHookDockerRemoteAction(args)
	case "kubectl":
		return eventHookKubectlRemoteAction(args)
	case "helm":
		return eventHookHelmRemoteAction(args)
	case "terraform", "tofu":
		return eventHookTerraformRemoteAction(name, args)
	case "pulumi":
		return eventHookPulumiRemoteAction(args)
	}

	return "", ""
}

func eventHookDockerRemoteAction(args []string) (action autonomy.Action, detail string) {
	args = stripEventHookPackageGlobalOptions(args)
	subcommand := firstEventHookSubcommand(args)

	switch subcommand {
	case eventHookSubcommandPush:
		return autonomy.ActionRemoteMutation, "docker push"
	case "compose":
		if len(args) > 1 && firstEventHookSubcommand(args[1:]) == eventHookSubcommandPush {
			return autonomy.ActionRemoteMutation, "docker compose push"
		}
	case "image":
		if len(args) > 1 && firstEventHookSubcommand(args[1:]) == eventHookSubcommandPush {
			return autonomy.ActionRemoteMutation, "docker image push"
		}
	case "buildx":
		if len(args) > 1 {
			buildxSubcommand := firstEventHookSubcommand(args[1:])
			switch {
			case buildxSubcommand == "imagetools":
				return autonomy.ActionRemoteMutation, "docker buildx imagetools"
			case buildxSubcommand == "build" && containsEventHookFlag(args[2:], "--push"):
				return autonomy.ActionRemoteMutation, "docker buildx build --push"
			}
		}
	}

	return "", ""
}

func eventHookKubectlRemoteAction(args []string) (action autonomy.Action, detail string) {
	subcommand := firstEventHookSubcommand(stripEventHookKubectlGlobalOptions(args))
	switch subcommand {
	case "apply", "annotate", "cordon", eventHookSubcommandCreate, eventHookSubcommandDelete, "drain", "edit", "exec",
		"label", "patch", "replace", "rollout", "run", "scale", "taint", "uncordon":
		return autonomy.ActionRemoteMutation, "kubectl " + subcommand
	default:
		return "", ""
	}
}

func eventHookHelmRemoteAction(args []string) (action autonomy.Action, detail string) {
	subcommand := firstEventHookSubcommand(stripEventHookPackageGlobalOptions(args))
	switch subcommand {
	case eventHookSubcommandDelete, "install", eventHookSubcommandPush, eventHookSubcommandRollback, "uninstall", "upgrade":
		return autonomy.ActionRemoteMutation, "helm " + subcommand
	default:
		return "", ""
	}
}

func eventHookTerraformRemoteAction(name string, args []string) (action autonomy.Action, detail string) {
	subcommand := firstEventHookSubcommand(stripEventHookPackageGlobalOptions(args))
	switch subcommand {
	case "apply", "destroy", "import", "taint", "untaint":
		return autonomy.ActionRemoteMutation, name + " " + subcommand
	default:
		return "", ""
	}
}

func eventHookPulumiRemoteAction(args []string) (action autonomy.Action, detail string) {
	subcommand := firstEventHookSubcommand(stripEventHookPackageGlobalOptions(args))
	switch subcommand {
	case "destroy", "import", "refresh", "up":
		return autonomy.ActionRemoteMutation, "pulumi " + subcommand
	default:
		return "", ""
	}
}

func stripEventHookPackageGlobalOptions(args []string) []string {
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

		if eventHookOptionTakesValue(arg) && len(args) > 1 {
			args = args[2:]
			continue
		}

		args = args[1:]
	}

	return nil
}

func stripEventHookKubectlGlobalOptions(args []string) []string {
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

		if eventHookKubectlGlobalOptionTakesValue(arg) && len(args) > 1 {
			args = args[2:]
			continue
		}

		args = args[1:]
	}

	return nil
}

func firstEventHookSubcommand(args []string) string {
	for _, arg := range args {
		arg = strings.TrimSpace(arg)
		if arg == "" || strings.HasPrefix(arg, "-") {
			continue
		}

		return strings.ToLower(arg)
	}

	return ""
}

func eventHookSubcommandMutates(args []string) bool {
	switch firstEventHookSubcommand(args) {
	case "add", eventHookSubcommandCreate, eventHookSubcommandDelete, "remove", "rm", "set":
		return true
	default:
		return false
	}
}

func containsEventHookFlag(args []string, flag string) bool {
	for _, arg := range args {
		lower := strings.ToLower(strings.TrimSpace(arg))
		if lower == flag || strings.HasPrefix(lower, flag+"=") {
			return true
		}
	}

	return false
}

func eventHookOptionTakesValue(arg string) bool {
	arg = strings.ToLower(strings.TrimSpace(arg))
	if strings.Contains(arg, "=") {
		return false
	}

	switch arg {
	case "-c", "-f", "-n", "-p", "-r", "-t", "-u",
		"--config", "--config-file", "--context", "--cwd", "--file", "--filename",
		"--kubeconfig", eventHookArgNamespace, "--profile", "--registry", "--repo",
		"--repository", "--server", "--token", "--user":
		return true
	default:
		return false
	}
}

func eventHookKubectlGlobalOptionTakesValue(arg string) bool {
	arg = strings.ToLower(strings.TrimSpace(arg))
	if strings.Contains(arg, "=") {
		return false
	}

	switch arg {
	case "--as", "--as-group", "--cache-dir", "--certificate-authority",
		"--client-certificate", "--client-key", "--cluster", "--context",
		"--kubeconfig", eventHookArgNamespace, "--password", "--profile", "--request-timeout",
		"--server", "--tls-server-name", "--token", "--user", "--username", "-n", "-s":
		return true
	default:
		return false
	}
}

func eventHookSSHStartsRemoteSession(args []string) bool {
	for _, arg := range args {
		arg = strings.TrimSpace(arg)
		if arg == "" {
			continue
		}

		lower := strings.ToLower(arg)
		if lower == "-v" || lower == "-vv" || lower == "-vvv" || lower == "-q" || lower == "-t" || lower == "-tt" {
			continue
		}

		if eventHookOptionTakesValue(lower) {
			continue
		}

		if strings.HasPrefix(arg, "-") {
			continue
		}

		return true
	}

	return false
}

func eventHookHTTPMutatesRemote(name string, args []string) bool {
	switch name {
	case "curl":
		return eventHookCurlMutatesRemote(args)
	case "wget":
		return eventHookWgetMutatesRemote(args)
	case "http", "https":
		return eventHookHTTPieMutatesRemote(args)
	default:
		return false
	}
}

func eventHookHTTPMergesPullRequest(name string, args []string) bool {
	return eventHookHTTPMutatesRemote(name, args) && eventHookArgsContainPullRequestMergePath(args)
}

func eventHookArgsContainPullRequestMergePath(args []string) bool {
	for _, arg := range args {
		arg = strings.ToLower(strings.TrimSpace(arg))
		if strings.Contains(arg, "mergepullrequest") ||
			strings.Contains(arg, "enablepullrequestautomerge") ||
			(strings.Contains(arg, "/pulls/") && strings.Contains(arg, "/merge")) {
			return true
		}
	}

	return false
}

func eventHookCurlMutatesRemote(args []string) bool {
	method, explicitMethod := eventHookCurlHTTPMethod(args)
	if eventHookHTTPMethodMutates(method) {
		return true
	}

	if eventHookHTTPMethodReads(method) {
		return false
	}

	if explicitMethod {
		return strings.TrimSpace(method) != ""
	}

	return eventHookCurlSendsBody(args) || eventHookCurlUploads(args)
}

func eventHookCurlHTTPMethod(args []string) (string, bool) {
	for i := range args {
		raw := strings.TrimSpace(args[i])
		lower := strings.ToLower(raw)

		if raw == "-X" || lower == "--request" {
			if i+1 < len(args) {
				return strings.ToUpper(strings.TrimSpace(args[i+1])), true
			}

			return "", false
		}

		if strings.HasPrefix(raw, "-X") && len(raw) > len("-X") {
			return strings.ToUpper(strings.TrimSpace(raw[len("-X"):])), true
		}

		if strings.HasPrefix(lower, "--request=") {
			_, value, _ := strings.Cut(raw, "=")
			return strings.ToUpper(strings.TrimSpace(value)), true
		}
	}

	return "", false
}

func eventHookCurlSendsBody(args []string) bool {
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

func eventHookCurlUploads(args []string) bool {
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

func eventHookWgetMutatesRemote(args []string) bool {
	method, explicitMethod := eventHookWgetHTTPMethod(args)
	if eventHookHTTPMethodMutates(method) {
		return true
	}

	if eventHookHTTPMethodReads(method) {
		return false
	}

	if explicitMethod {
		return strings.TrimSpace(method) != ""
	}

	return eventHookWgetSendsBody(args)
}

func eventHookWgetHTTPMethod(args []string) (string, bool) {
	for i := range args {
		arg := strings.TrimSpace(args[i])
		lower := strings.ToLower(arg)

		if lower == "--method" {
			if i+1 < len(args) {
				return strings.ToUpper(strings.TrimSpace(args[i+1])), true
			}

			return "", true
		}

		if strings.HasPrefix(lower, "--method=") {
			_, value, _ := strings.Cut(arg, "=")
			return strings.ToUpper(strings.TrimSpace(value)), true
		}
	}

	return "", false
}

func eventHookWgetSendsBody(args []string) bool {
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

func eventHookHTTPieMutatesRemote(args []string) bool {
	seenTarget := false

	for i := 0; i < len(args); i++ {
		arg := strings.TrimSpace(args[i])
		if arg == "" {
			continue
		}

		if strings.HasPrefix(arg, "-") {
			if eventHookHTTPieOptionTakesValue(arg) && i+1 < len(args) {
				i++
			}

			continue
		}

		method := strings.ToUpper(arg)
		if eventHookHTTPieMethodToken(method) {
			return !eventHookHTTPMethodReads(method)
		}

		if !seenTarget {
			seenTarget = true
			continue
		}

		if eventHookHTTPieRequestItemSendsBody(arg) {
			return true
		}
	}

	return false
}

func eventHookHTTPieMethodToken(method string) bool {
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

func eventHookHTTPieOptionTakesValue(arg string) bool {
	arg = strings.ToLower(strings.TrimSpace(arg))
	switch arg {
	case "--session", "--session-read-only", "--auth", "-a", "--proxy", "--verify", "--cert", "--cert-key", "--timeout":
		return true
	default:
		return false
	}
}

func eventHookHTTPieRequestItemSendsBody(arg string) bool {
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

func eventHookHTTPMethodMutates(method string) bool {
	switch strings.ToUpper(strings.TrimSpace(method)) {
	case httpMethodPost, httpMethodPut, httpMethodPatch, httpMethodDelete:
		return true
	default:
		return false
	}
}

func eventHookHTTPMethodReads(method string) bool {
	switch strings.ToUpper(strings.TrimSpace(method)) {
	case "GET", "HEAD", "OPTIONS":
		return true
	default:
		return false
	}
}
