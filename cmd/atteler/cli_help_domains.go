package main

import (
	"strconv"

	"github.com/tommoulard/atteler/pkg/watch"
)

const (
	bashCommandName = "bash"
	helpCommandName = "help"
	helpLongFlag    = "--help"
	helpGoFlag      = "-help"
	helpShortFlag   = "-h"
)

type cliCommandAlias struct {
	Name             string
	Summary          string
	Args             string
	Legacy           []string
	Aliases          []string
	JoinArgs         bool
	PromptAfterValue bool
	PromptFromStdin  bool
	// OpaqueArgs treats every token after the grouped command as command data.
	// Use it for shell-like commands where dash-prefixed tokens belong to the
	// command instead of Atteler flag parsing.
	OpaqueArgs bool
}

type cliHelpDomain struct {
	Name     string
	Title    string
	Summary  string
	Aliases  []string
	Commands []cliCommandAlias
	Examples []string
}

//nolint:govet // field order keeps the result fields grouped for readability.
type cliArgPlan struct {
	Args       []string
	HelpDomain string
	Err        error
	Help       bool
}

var cliHelpDomains = []cliHelpDomain{
	{
		Name:    "chat/session",
		Title:   "Chat & sessions",
		Summary: "Run the TUI or one-shot prompts, manage saved sessions, transcripts, headless runs, and artifacts.",
		Aliases: []string{"chat", "session", "sessions"},
		Commands: []cliCommandAlias{
			{Name: "run", Args: "[prompt]", Summary: "start chat or run positional one-shot prompt text", JoinArgs: true},
			{Name: "once", Args: "<prompt|--stdin>", Summary: "send one prompt and exit", Legacy: []string{"--once"}, JoinArgs: true, PromptFromStdin: true},
			{Name: "list", Summary: "list saved sessions", Legacy: []string{"--list-sessions"}},
			{Name: "tags", Summary: "list saved session tags", Legacy: []string{"--list-session-tags"}},
			{Name: "search", Args: "<query>", Summary: "search saved session metadata and transcripts", Legacy: []string{"--search-sessions"}, JoinArgs: true},
			{Name: "show", Args: "<id-or-path>", Summary: "print one saved session as YAML", Legacy: []string{"--show-session"}},
			{Name: "summary", Args: "<id-or-path>", Summary: "print compact saved session metadata", Legacy: []string{"--session-summary"}},
			{Name: "replay", Args: "<id-or-path>", Summary: "print a previous transcript", Legacy: []string{"--replay"}},
			{Name: "export", Args: "<id-or-path>", Summary: "export a previous transcript", Legacy: []string{"--export-session"}},
			{Name: "messages", Summary: "list compact message records for --session", Legacy: []string{"--list-messages"}},
			{Name: "artifacts", Summary: "list artifact records for --session", Legacy: []string{"--list-artifacts"}},
			{Name: "failures", Summary: "list negative-knowledge records for --session", Legacy: []string{"--list-failures"}},
			{Name: "record-failure", Args: "<approach>", Summary: "record negative knowledge on --session", Legacy: []string{"--record-failure"}, JoinArgs: true},
			{Name: "record-artifact", Args: "<path>", Summary: "record a useful artifact path on --session", Legacy: []string{"--record-artifact"}},
			{Name: "merge-artifacts", Args: "<path>", Summary: "merge selected-session text artifacts into Markdown", Legacy: []string{"--merge-artifacts"}},
			{Name: "headless", Summary: "list active headless runs", Legacy: []string{"--list-headless"}},
			{Name: "stream-headless", Args: "<id>", Summary: "stream one headless run log", Legacy: []string{"--stream-headless"}},
		},
		Examples: []string{
			`atteler chat once "Explain this repository in one paragraph"`,
			`atteler session list`,
			`atteler session search "auth retry"`,
		},
	},
	{
		Name:    "config",
		Title:   "Configuration & diagnostics",
		Summary: "Inspect config load order, templates, validation, lifecycle hooks, readiness, and version metadata.",
		Aliases: []string{"cfg", "diagnostics", "diag"},
		Commands: []cliCommandAlias{
			{Name: "paths", Summary: "list config files in load order", Legacy: []string{"--list-config-paths"}},
			{Name: "template", Summary: "print a starter YAML config", Legacy: []string{"--print-config-template"}},
			{Name: "init", Args: "<path>", Summary: "write a starter YAML config without overwriting", Legacy: []string{"--init-config"}},
			{Name: "validate", Summary: "validate merged YAML/JSON config", Legacy: []string{"--validate-config"}},
			{Name: "doctor", Summary: "run provider-aware readiness diagnostics", Legacy: []string{"--doctor"}},
			{Name: "doctor-offline", Summary: "run offline readiness diagnostics", Legacy: []string{"--doctor-offline"}},
			{Name: "hooks", Summary: "list supported lifecycle hook events", Legacy: []string{"--list-hook-events"}},
			{Name: "hooks-json", Summary: "list hook events as JSON", Legacy: []string{"--list-hook-events-json"}},
			{Name: "version", Summary: "print build version information", Legacy: []string{"--version"}},
		},
		Examples: []string{
			`atteler config paths`,
			`atteler config validate`,
			`atteler config doctor-offline`,
		},
	},
	{
		Name:    "providers",
		Title:   "Providers & models",
		Summary: "Choose models, inspect provider inventories, tune generation settings, and preview routing decisions.",
		Aliases: []string{"provider"},
		Commands: []cliCommandAlias{
			{Name: "list", Summary: "list built-in provider names without API calls", Legacy: []string{"--list-providers"}},
			{Name: "known-models", Summary: "list built-in provider/model IDs without API calls", Legacy: []string{"--list-known-models"}},
			{Name: "models", Summary: "list models discovered from configured providers", Legacy: []string{"--list-models"}},
			{Name: "route-interactive", Summary: "rank --route-candidate values for low TTFT", Legacy: []string{"--route-interactive"}},
			{Name: "route-batch", Summary: "rank --route-candidate values for batch/cost preference", Legacy: []string{"--route-batch"}},
		},
		Examples: []string{
			`atteler providers list`,
			`atteler providers known-models`,
			`atteler providers models`,
		},
	},
	{
		Name:    "agents",
		Title:   "Agents & orchestration",
		Summary: "Inspect agents, plan matching agents, manage task lists, spawn sub-agents, and preview speculative/async workflows.",
		Aliases: []string{"agent", "orchestration"},
		Commands: []cliCommandAlias{
			{Name: "list", Summary: "list configured agents", Legacy: []string{"--list-agents"}},
			{Name: "performance", Summary: "summarize saved-session agent performance", Legacy: []string{"--agent-performance-summary"}},
			{Name: "describe", Args: "<name>", Summary: "print one configured agent as YAML", Legacy: []string{"--describe-agent"}},
			{Name: "plan", Args: "<prompt>", Summary: "preview configured agents for a prompt", Legacy: []string{"--plan-agents"}, JoinArgs: true},
			{Name: "prompt-complete", Args: "<input>", Summary: "preview deterministic prompt completions", Legacy: []string{"--prompt-complete"}, JoinArgs: true},
			{Name: "task-list", Summary: "list persistent agent tasks", Legacy: []string{"--task-list"}},
			{Name: "task-add", Args: "<title>", Summary: "add a persistent agent task", Legacy: []string{"--task-add"}, JoinArgs: true},
			{Name: "task-assign", Args: "<id:agent>", Summary: "assign a persistent task", Legacy: []string{"--task-assign"}},
			{Name: "task-complete", Args: "<id>", Summary: "mark a persistent task complete", Legacy: []string{"--task-complete"}},
			{Name: "async-plan", Summary: "print dependency-aware async task batches", Legacy: []string{"--async-plan"}},
			{Name: "async-run", Summary: "execute dependency-aware async tasks", Legacy: []string{"--async-run"}},
			{Name: "spawn", Args: "<agent|prompt>", Summary: "spawn sub-agent one-shot prompts", Legacy: []string{"--spawn-agent"}, JoinArgs: true},
			{Name: "speculate-plan", Summary: "print a speculative three-round plan", Legacy: []string{"--speculate-plan"}},
			{Name: "speculate-run", Summary: "execute the speculative three-round pipeline", Legacy: []string{"--speculate-run"}},
			{Name: "feedback", Summary: "derive improvement proposals from --session", Legacy: []string{"--feedback-proposals"}},
			{Name: "feedback-apply", Args: "<config>", Summary: "apply selected-session feedback proposals to agent config", Legacy: []string{"--feedback-apply-config"}},
			{Name: "skill-suggest", Args: "<step>", Summary: "suggest reusable skills from repeated observed actions", Legacy: []string{"--skill-step"}, JoinArgs: true},
			{Name: bashCommandName, Args: "<command>", Summary: "run an explicit local bash command", Legacy: []string{"--bash"}, JoinArgs: true, OpaqueArgs: true},
		},
		Examples: []string{
			`atteler agents list`,
			`atteler agents plan "review auth changes"`,
			`atteler agents task-list`,
		},
	},
	{
		Name:    "memory/rag",
		Title:   "Memory & RAG",
		Summary: "Search saved sessions, UTF-8 file memory stores, agent vector memory, local vector indexes, and git history.",
		Aliases: []string{"memory", "rag", "mem"},
		Commands: []cliCommandAlias{
			{Name: "search", Args: "<query>", Summary: "search local memory", Legacy: []string{"--memory-search"}, JoinArgs: true},
			{Name: "index", Args: "<file>", Summary: "add a file to the local memory store", Legacy: []string{"--memory-index"}},
			{Name: "agent-search", Args: "<query>", Summary: "search one agent's vector memory", Legacy: []string{"--agent-memory-search"}, JoinArgs: true},
			{Name: "agent-index", Args: "<file>", Summary: "add a file to one agent's vector memory", Legacy: []string{"--agent-memory-index"}},
			{Name: "vector-search", Args: "<query>", Summary: "search dependency-free local vector indexes", Legacy: []string{"--vector-search"}, JoinArgs: true},
			{Name: "vector-index", Args: "<file>", Summary: "add a file to dependency-free vector search", Legacy: []string{"--vector-index"}},
			{Name: "git-history", Args: "<query>", Summary: "search local git history subjects/files/authors", Legacy: []string{"--git-history-search"}, JoinArgs: true},
			{Name: "context-pack", Args: "<path>", Summary: "compact a role-prefixed transcript file", Legacy: []string{"--context-pack-file"}},
		},
		Examples: []string{
			`atteler memory search "OAuth retry storm"`,
			`atteler memory git-history "memory regression"`,
			`atteler memory vector-search "redirect risks"`,
		},
	},
	{
		Name:    "code-intel",
		Title:   "Code intelligence",
		Summary: "Run Go code index, import graph, package/file/symbol, impact, and optional LSP queries without an LLM call.",
		Aliases: []string{"code", "codeintel", "code-intelligence"},
		Commands: []cliCommandAlias{
			{Name: "summary", Summary: "print compact Go code index counts", Legacy: []string{"--code-summary"}},
			{Name: "files", Summary: "list Go files with package/import/symbol counts", Legacy: []string{"--code-files"}},
			{Name: "packages", Summary: "list Go packages with file/symbol counts", Legacy: []string{"--code-packages"}},
			{Name: "package", Args: "<package>", Summary: "list files and symbol counts for one package", Legacy: []string{"--code-package"}},
			{Name: "package-imports", Args: "<package>", Summary: "list import usage counts for one package", Legacy: []string{"--code-package-imports"}},
			{Name: "package-symbols", Args: "<package>", Summary: "list symbol kind counts for one package", Legacy: []string{"--code-package-symbols"}},
			{Name: "file", Args: "<path>", Summary: "print package, symbols, and imports for one file", Legacy: []string{"--code-file"}},
			{Name: "file-imports", Args: "<path>", Summary: "list imports for one Go file", Legacy: []string{"--code-file-imports"}},
			{Name: "file-symbols", Args: "<path>", Summary: "list symbols for one Go file", Legacy: []string{"--code-file-symbols"}},
			{Name: "symbol", Args: "<name>", Summary: "find Go symbols by exact name", Legacy: []string{"--code-symbol"}},
			{Name: "symbol-prefix", Args: "<prefix>", Summary: "find Go symbols by prefix", Legacy: []string{"--code-symbol-prefix"}},
			{Name: "symbol-kind", Args: "<kind>", Summary: "list Go symbols by kind", Legacy: []string{"--code-symbol-kind"}},
			{Name: "imports", Summary: "list Go import edges", Legacy: []string{"--code-imports"}},
			{Name: "import-summary", Summary: "list import paths with usage counts", Legacy: []string{"--code-import-summary"}},
			{Name: "import-path", Args: "<path>", Summary: "list files importing one exact path", Legacy: []string{"--code-import-path"}},
			{Name: "import-prefix", Args: "<prefix>", Summary: "list files importing paths with one prefix", Legacy: []string{"--code-import-prefix"}},
			{Name: "layers", Summary: "list topological Go import graph layers", Legacy: []string{"--code-layers"}},
			{Name: "cycles", Summary: "list Go import graph cycles", Legacy: []string{"--code-cycles"}},
			{Name: "impact", Args: "<path>", Summary: "list files impacted by an import path", Legacy: []string{"--code-impact"}},
			{Name: "reachable", Args: "<path>", Summary: "list reachable import graph nodes", Legacy: []string{"--code-reachable"}},
			{Name: "deps", Args: "<path>", Summary: "list direct import graph dependencies", Legacy: []string{"--code-deps"}},
			{Name: "rdeps", Args: "<path>", Summary: "list direct reverse dependencies", Legacy: []string{"--code-rdeps"}},
			{Name: "lsp-symbols", Summary: "request document symbols from an external LSP", Legacy: []string{"--lsp-symbols"}},
			{Name: "lsp-workspace", Args: "<query>", Summary: "request workspace symbols from an external LSP", Legacy: []string{"--lsp-workspace-symbols"}},
		},
		Examples: []string{
			`atteler code-intel summary`,
			`atteler code-intel symbol NewRegistry`,
			`atteler code-intel import-prefix github.com/tommoulard/atteler/pkg/`,
		},
	},
	{
		Name:    "review",
		Title:   "Review",
		Summary: "Scan the current repository or plan/run review-agent workflows with explicit gates.",
		Aliases: []string{"reviews"},
		Commands: []cliCommandAlias{
			{Name: "scan", Summary: "scan the current repository and print a structured review report", Legacy: []string{"--review-scan"}},
			{Name: "plan", Summary: "print speculative review-agent plan", Legacy: []string{"--review-plan"}},
			{Name: "run", Summary: "execute the review-agent three-round pipeline", Legacy: []string{"--review-run"}},
		},
		Examples: []string{
			`atteler review scan`,
			`atteler review plan`,
			`atteler review run`,
		},
	},
	{
		Name:    "watch",
		Title:   "Watch",
		Summary: "Scan or continuously watch repository health findings for background-agent workflows.",
		Aliases: []string{"watcher"},
		Commands: []cliCommandAlias{
			{Name: "scan", Summary: "scan once for background-agent health findings", Legacy: []string{"--watch-scan"}},
			{Name: "json", Summary: "scan once and emit findings as JSON", Legacy: []string{"--watch-scan", "--watch-json"}},
			{Name: "loop", Summary: "continuously scan until interrupted or max iterations", Legacy: []string{"--watch-loop"}},
		},
		Examples: []string{
			`atteler watch scan`,
			`atteler watch json`,
			`atteler watch loop`,
		},
	},
	{
		Name:    "plugins",
		Title:   "Plugins & MCP",
		Summary: "Inspect, run, scaffold plugins, and validate or invoke MCP manifests.",
		Aliases: []string{"plugin"},
		Commands: []cliCommandAlias{
			{Name: "list", Summary: "list configured local plugin manifests", Legacy: []string{"--list-plugins"}},
			{Name: "describe", Args: "<name>", Summary: "print one configured plugin manifest", Legacy: []string{"--describe-plugin"}},
			{Name: "run", Args: "<plugin[/entrypoint]>", Summary: "execute a configured plugin entrypoint", Legacy: []string{"--run-plugin"}},
			{Name: "init-rtk", Args: "<dir>", Summary: "write an RTK plugin scaffold", Legacy: []string{"--init-rtk-plugin"}},
			{Name: "mcp-manifest", Args: "<path>", Summary: "validate/list an MCP manifest", Legacy: []string{"--mcp-manifest"}, Aliases: []string{"manifest"}},
			{Name: "mcp-tool", Args: "<tool>", Summary: "invoke an MCP tool through tools/call", Legacy: []string{"--mcp-tool"}, Aliases: []string{"tool"}},
			{Name: "mcp-method", Args: "<method>", Summary: "invoke a JSON-RPC method on --mcp-server", Legacy: []string{"--mcp-method"}, Aliases: []string{"method"}},
		},
		Examples: []string{
			`atteler plugins list`,
			`atteler plugins run reviewer/check`,
			`atteler plugins manifest .atteler/mcp.yaml`,
		},
	},
	{
		Name:    "worktrees",
		Title:   "Worktrees",
		Summary: "Run sessions in isolated git worktrees and list or merge existing session worktrees.",
		Aliases: []string{"worktree", "wt"},
		Commands: []cliCommandAlias{
			{Name: "run", Args: "[prompt]", Summary: "enable worktree isolation for this session", Legacy: []string{"--worktree"}, JoinArgs: true},
			{Name: "list", Summary: "list active atteler worktrees", Legacy: []string{"--list-worktrees"}},
			{Name: "merge", Args: "<session-id>", Summary: "merge a session worktree back into its base branch", Legacy: []string{"--merge-worktree"}},
		},
		Examples: []string{
			`atteler worktrees run "Add unit tests for auth"`,
			`atteler worktrees list`,
			`atteler worktrees merge 20260430-120000-deadbeef`,
		},
	},
	{
		Name:    "eval",
		Title:   "Evaluation & fixtures",
		Summary: "Compare deterministic outputs, record/replay one-shot response fixtures, and record agent evaluations.",
		Aliases: []string{"evaluation", "evaluations"},
		Commands: []cliCommandAlias{
			{Name: "output", Args: "<path>", Summary: "compare actual output against expected text/file", Legacy: []string{"--eval-output"}},
			{Name: "list", Summary: "list agent evaluations on --session", Legacy: []string{"--list-evaluations"}},
			{Name: "record", Args: "<agent>", Summary: "append an evaluation to --session", Legacy: []string{"--record-evaluation"}},
			{Name: "record-response", Args: "<path> <prompt|--stdin>", Summary: "run one prompt and write request/response JSON", Legacy: []string{"--record-response"}, PromptAfterValue: true},
			{Name: "replay-response", Args: "<path> <prompt|--stdin>", Summary: "run one prompt from a recorded response JSON", Legacy: []string{"--replay-response"}, PromptAfterValue: true},
		},
		Examples: []string{
			`atteler eval output .atteler/fixtures/readme-summary.txt --eval-expected "package overview"`,
			`atteler eval record reviewer`,
			`atteler eval replay-response .atteler/fixtures/once.json "Summarize @README.md"`,
		},
	},
}

var implicitFlagDefaults = map[string]string{
	"agent-memory-limit":       "5",
	"bash-timeout-seconds":     "120",
	"context-pack-tokens":      "unlimited",
	"git-history-limit":        "5",
	"mcp-timeout-seconds":      "none",
	"memory-limit":             "5",
	"merge-artifact-max-bytes": strconv.FormatInt(watch.DefaultLargeFileBytes, 10),
	"plan-max-agents":          "unlimited",
	"plugin-timeout-seconds":   "30",
	"prompt-complete-limit":    "5",
	"skill-max-steps":          "6",
	"skill-min-occurrences":    "2",
	"spawn-timeout-seconds":    "none",
	"vector-limit":             "5",
	"watch-interval-seconds":   "60",
	"watch-large-file-bytes":   strconv.FormatInt(watch.DefaultLargeFileBytes, 10),
	"watch-max-iterations":     "unlimited",
}
