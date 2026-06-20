package main

import (
	"strconv"

	"github.com/tommoulard/atteler/pkg/watch"
)

const (
	bashCommandName        = "bash"
	autoresearchDomainName = "autoresearch"
	helpCommandName        = "help"
	issueCommandName       = "issue"
	researchDomainName     = "research"
	scoutDomainName        = "scout"
	sessionCommandName     = "session"
	helpLongFlag           = "--help"
	helpGoFlag             = "-help"
	helpShortFlag          = "-h"

	agentMemoryIndexFlag   = "--agent-memory-index"
	agentMemoryDeleteFlag  = "--agent-memory-delete"
	agentMemoryCompactFlag = "--agent-memory-compact"
	agentMemoryMigrateFlag = "--agent-memory-migrate"
)

type cliCommandAlias struct {
	Name             string
	Summary          string
	Args             string
	TextOutput       string
	JSONSchema       string
	Legacy           []string
	Aliases          []string
	Examples         []string
	JSONFields       []string
	JoinArgs         bool
	PromptAfterValue bool
	PromptFromStdin  bool
	// OpaqueArgs treats every token after the grouped command as command data.
	// Use it for shell-like commands where dash-prefixed tokens belong to the
	// command instead of Atteler flag parsing.
	OpaqueArgs bool
}

type cliHelpDomain struct {
	Name            string
	Title           string
	Summary         string
	Aliases         []string
	HiddenAliases   []string
	Commands        []cliCommandAlias
	RoutingCommands []cliCommandAlias
	Examples        []string
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
		Summary: "Run the TUI or one-shot prompts, manage saved sessions, transcripts, headless runs, artifacts, and multi-agent run records.",
		Aliases: []string{"chat", helpSelectorSession, "sessions"},
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
			{Name: "runs", Summary: "list review/speculation runs for --session", Legacy: []string{"--list-runs"}},
			{Name: "show-run", Args: "<id|latest|review|speculation>", Summary: "show one review/speculation run as YAML", Legacy: []string{"--show-run"}},
			{Name: "export-run", Args: "<id|latest|review|speculation>", Summary: "export one review/speculation run", Legacy: []string{"--export-run"}},
			{Name: "replay-run", Args: "<id|latest|review|speculation>", Summary: "replay recorded run artifacts without provider calls", Legacy: []string{"--replay-run"}},
			{Name: "resume-run", Args: "<id|latest|review|speculation>", Summary: "resume from recorded run artifacts without provider calls", Legacy: []string{"--resume-run"}},
			{Name: "messages", Summary: "list compact message records for --session", Legacy: []string{"--list-messages"}},
			{Name: "artifacts", Summary: "list artifact records for --session", Legacy: []string{"--list-artifacts"}},
			{Name: "failures", Summary: "list negative-knowledge records for --session", Legacy: []string{"--list-failures"}},
			{Name: "record-failure", Args: "<approach>", Summary: "record negative knowledge on --session", Legacy: []string{"--record-failure"}, JoinArgs: true},
			{Name: "record-artifact", Args: "<path>", Summary: "record a useful artifact path on --session", Legacy: []string{"--record-artifact"}},
			{Name: "merge-artifacts", Args: "<path>", Summary: "merge selected-session artifacts into a provenance bundle", Legacy: []string{"--merge-artifacts"}},
			{Name: "headless", Summary: "list headless runs; filter with --headless-status/--headless-max-age", Legacy: []string{"--list-headless"}},
			{Name: "status-headless", Args: "<id>", Summary: "show one headless run status", Legacy: []string{"--status-headless"}},
			{Name: "cancel-headless", Args: "<id>", Summary: "cancel one live headless run", Legacy: []string{"--cancel-headless"}},
			{Name: "retry-headless", Args: "<id>", Summary: "retry one terminal headless run", Legacy: []string{"--retry-headless"}},
			{Name: "recover-headless", Summary: "reconcile stale/orphaned/expired headless runs", Legacy: []string{"--recover-headless"}},
			{Name: "stream-headless", Args: "<id>", Summary: "stream one headless run log", Legacy: []string{"--stream-headless"}},
			{Name: "cleanup-headless", Summary: "remove terminal headless runs older than --headless-max-age", Legacy: []string{"--cleanup-headless"}},
		},
		Examples: []string{
			`atteler chat once "Explain this repository in one paragraph"`,
			`atteler session list`,
			`atteler session search "auth retry"`,
		},
	},
	{
		Name:    configDomainName,
		Title:   "Configuration & diagnostics",
		Summary: "Inspect config load order, templates, validation, lifecycle hooks, readiness, and version metadata.",
		Aliases: []string{"cfg", "diagnostics", "diag"},
		Commands: []cliCommandAlias{
			{Name: "paths", Summary: "list config files in load order", Legacy: []string{"--list-config-paths"}},
			{Name: "template", Summary: "print a starter YAML config", Legacy: []string{"--print-config-template"}},
			{Name: "init", Args: "<path>", Summary: "write a starter YAML config without overwriting", Legacy: []string{"--init-config"}},
			{Name: "validate", Summary: "validate merged YAML/JSON config and importer warnings", Legacy: []string{"--validate-config"}},
			{Name: "migrate", Summary: "migrate existing config/state files to the current schema", Legacy: []string{"--config-migrate"}},
			{Name: "report", Summary: "print redacted config diagnostics for issue reports", Legacy: []string{"--config-report"}},
			{Name: "explain", Args: "[field-prefix]", Summary: "print merged config values with per-field provenance", Legacy: []string{"--explain-config"}},
			{Name: "doctor", Summary: "run provider-aware readiness diagnostics", Legacy: []string{"--doctor"}},
			{Name: "doctor-offline", Summary: "run offline readiness diagnostics; use --output json for CI", Legacy: []string{"--doctor-offline"}},
			{Name: "state", Summary: "show interactive state path, revision, and selected preference sources", Legacy: []string{"--state-diagnostics"}},
			{Name: "commands-json", Summary: "dump the inspectable CLI command surface as JSON", Legacy: []string{"--command-surface-json"}},
			{Name: "commands-docs", Summary: "render command surface docs from the dispatch contract", Legacy: []string{"--command-surface-docs"}},
			{Name: "hooks", Summary: "list supported lifecycle hook events", Legacy: []string{"--list-hook-events"}},
			{Name: "hooks-json", Summary: "list hook events as JSON", Legacy: []string{"--list-hook-events-json"}},
			{Name: "version", Summary: "print build version information", Legacy: []string{"--version"}},
		},
		Examples: []string{
			`atteler config paths`,
			`atteler config validate`,
			`atteler config migrate`,
			`atteler config report`,
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
			{Name: "resolve", Summary: "explain how a model ID resolves to a provider", Args: "<model>", Legacy: []string{"--explain-model-resolution"}},
			{Name: commandOllamaStatus, Summary: "print Ollama daemon lifecycle status without starting it", Legacy: []string{"--ollama-status"}},
			{Name: commandOllamaStop, Summary: "stop and clean up an Atteler-owned Ollama daemon", Legacy: []string{"--ollama-stop"}},
			{Name: "route-interactive", Summary: "rank --route-candidate values for low TTFT", Legacy: []string{"--route-interactive"}},
			{Name: "route-batch", Summary: "rank --route-candidate values for batch/cost preference", Legacy: []string{"--route-batch"}},
		},
		Examples: []string{
			`atteler providers list`,
			`atteler providers known-models`,
			`atteler providers models`,
			`atteler providers resolve gpt-5.5`,
			`atteler providers ` + commandOllamaStatus,
			`atteler providers ` + commandOllamaStop,
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
			{Name: "prompt-complete", Args: "<input>", Summary: "preview local prompt completions with context freshness", Legacy: []string{"--prompt-complete"}, JoinArgs: true},
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
			{Name: "feedback-apply", Args: "<config>", Summary: "record selected-session feedback proposals as pending guidance", Legacy: []string{"--feedback-apply-config"}},
			{Name: "feedback-approve", Args: "<config>", Summary: "approve pending feedback guidance with --feedback-approve-agent and --feedback-approve-id", Legacy: []string{"--feedback-approve-config"}},
			{Name: "feedback-rollback", Args: "<config>", Summary: "roll back feedback guidance with --feedback-rollback-agent and --feedback-rollback-id", Legacy: []string{"--feedback-rollback-config"}},
			{Name: "skill-suggest", Args: "<step>", Summary: "suggest reviewed skill directories from repeated workflows", Legacy: []string{"--skill-step"}, JoinArgs: true},
			{Name: "skill-learning-list", Summary: "list automatically generated skills", Legacy: []string{"--skill-learning-list"}},
			{Name: "skill-learning-show", Args: "<slug>", Summary: "print one automatically generated skill", Legacy: []string{"--skill-learning-show"}},
			{Name: "skill-learning-edit", Args: "<slug>", Summary: "open one generated skill in $VISUAL or $EDITOR", Legacy: []string{"--skill-learning-edit"}},
			{Name: "skill-learning-disable", Args: "<slug>", Summary: "disable updates for one generated skill", Legacy: []string{"--skill-learning-disable"}},
			{Name: "skill-learning-enable", Args: "<slug>", Summary: "enable updates for one generated skill", Legacy: []string{"--skill-learning-enable"}},
			{Name: "skill-learning-delete", Args: "<slug>", Summary: "delete one generated skill", Legacy: []string{"--skill-learning-delete"}},
			{Name: "skill-learning-disable-all", Summary: "opt out of automatic skill learning", Legacy: []string{"--skill-learning-disable-all"}},
			{Name: "skill-learning-enable-all", Summary: "enable automatic skill learning", Legacy: []string{"--skill-learning-enable-all"}},
			{Name: bashCommandName, Args: "<command>", Summary: "run an explicit local bash command", Legacy: []string{"--bash"}, JoinArgs: true, OpaqueArgs: true},
		},
		Examples: []string{
			`atteler agents list`,
			`atteler agents plan "review auth changes"`,
			`atteler agents task-list`,
		},
	},
	{
		Name:    researchDomainName,
		Title:   "Research",
		Summary: "Create local-first cited research run artifacts for technical decisions, architecture exploration, dependency evaluation, and planning.",
		Commands: []cliCommandAlias{
			{
				Name:     "run",
				Args:     "<question>",
				Summary:  "gather project guidance and supplied sources into a cited research report",
				Legacy:   []string{"--research-run"},
				JoinArgs: true,
			},
		},
		Examples: []string{
			`atteler research run "Compare approaches for plugin sandboxing in Go CLIs"`,
			`atteler research run --trusted-source go.dev --trusted-source github.com "Research best practices for safe agent worktrees"`,
			`atteler research run --output .atteler/research/plugin-sandboxing --generate-tasks "Find viable implementation approaches for sandboxing Atteler plugins"`,
		},
	},
	{
		Name:    scoutDomainName,
		Title:   "Scout",
		Summary: "Generate local-first product discovery, competitor inspiration, ranked roadmap ideas, and optional implementation tasks.",
		Commands: []cliCommandAlias{
			{
				Name:     "run",
				Args:     "<prompt>",
				Summary:  "inspect repository guidance and generate ranked feature or roadmap recommendations",
				Legacy:   []string{"--scout-run"},
				JoinArgs: true,
			},
		},
		Examples: []string{
			`atteler scout run "Find 10 feature ideas for Atteler based on current AI coding tools"`,
			`atteler scout run --competitors cursor,codex,openhands,aider,jules --generate-tasks "Identify features Atteler should add next"`,
			`atteler scout run --area autoresearch --tournament --variants 5 "Find improvements to Atteler's autoresearch workflow"`,
		},
	},
	{
		Name:    autoresearchDomainName,
		Title:   "Autoresearch",
		Summary: "Run a headless worktree loop that proposes code experiments, validates them, and keeps only improvements.",
		Commands: []cliCommandAlias{
			{
				Name:     "run",
				Args:     "<mission>",
				Summary:  "start an autonomous code experiment loop for a hard or long task",
				Legacy:   []string{"--autoresearch"},
				JoinArgs: true,
			},
		},
		Examples: []string{
			`atteler autoresearch run "Improve agent-loop recovery; keep only changes that pass make test"`,
			`atteler autoresearch run --tournament --variants 5 "Compare hypotheses for reducing prompt-context cache misses"`,
			`atteler autoresearch "Reduce prompt-context cache misses and validate with go test ./cmd/atteler"`,
			`atteler session headless`,
		},
	},
	{
		Name:    issueCommandName,
		Title:   "Issue implementation",
		Summary: "Run the autonomous Symphony issue-to-PR agent with explicit verification gates.",
		Aliases: []string{"issues"},
		Commands: []cliCommandAlias{
			{
				Name:    "implement",
				Args:    "<issue-ref>",
				Summary: "implement one tracker issue and optionally open a verified pull request",
				Legacy:  []string{"--issue-implement"},
			},
		},
		Examples: []string{
			`atteler issue implement GH-218 --open-pr`,
			`atteler issue implement https://github.com/owner/repo/issues/218 --open-pr`,
			`atteler issue implement GH-218 --open-pr --base main --run-tests --run-lint`,
		},
	},
	{
		Name:    "memory/retrieval",
		Title:   "Memory & Retrieval",
		Summary: "Search saved sessions, UTF-8 file memory stores, workspace vector indexes, agent vector memory, local lexical/embedding indexes, and git history.",
		Aliases: []string{"memory", "retrieval", "mem"},
		// Keep old RAG-shaped routes working without advertising lexical
		// fallback search as RAG in help output.
		HiddenAliases: []string{"rag", "memory/rag"},
		Commands: []cliCommandAlias{
			{Name: "search", Args: "<query>", Summary: "search scoped local memory and report the searched corpus", Legacy: []string{"--memory-search"}, JoinArgs: true},
			{Name: "retrieve", Args: "<query>", Summary: "search selected/filtered sources under the unified retrieval contract", Legacy: []string{"--retrieval-search"}, JoinArgs: true},
			{Name: "index", Args: "<file>", Summary: "add a file to the local memory store", Legacy: []string{"--memory-index"}},
			{Name: "purge", Args: "<selector>", Summary: "purge memory docs by session:<id>, tag:<tag>, repo:<path>, or all", Legacy: []string{"--memory-purge"}, JoinArgs: true},
			{Name: "rebuild", Summary: "rebuild the JSON memory store from the selected corpus", Legacy: []string{"--memory-rebuild"}},
			{Name: "list-corpus", Summary: "print memory corpus metadata", Legacy: []string{"--memory-list-corpus"}},
			{Name: "agent-search", Args: "<query>", Summary: "search one agent's vector memory", Legacy: []string{"--agent-memory-search"}, JoinArgs: true},
			{Name: "agent-index", Args: "<file>", Summary: "add a file to one agent's vector memory", Legacy: []string{agentMemoryIndexFlag}},
			{Name: "agent-delete", Args: "<id>", Summary: "delete one document from one agent's vector memory", Legacy: []string{agentMemoryDeleteFlag}},
			{Name: "agent-compact", Summary: "remove expired documents from per-agent vector memory", Legacy: []string{agentMemoryCompactFlag}},
			{Name: "agent-migrate", Summary: "explicitly migrate and re-vectorize per-agent memory", Legacy: []string{agentMemoryMigrateFlag}},
			{Name: "vector-search", Args: "<query>", Summary: "search a persisted lexical-fallback or embedding vector index", Legacy: []string{"--vector-search"}, JoinArgs: true},
			{Name: "vector-index", Args: "<file>", Summary: "chunk and add a file to the persisted vector index", Legacy: []string{"--vector-index"}},
			{Name: "git-history", Args: "<query>", Summary: "search local git history subjects/files/authors", Legacy: []string{"--git-history-search"}, JoinArgs: true},
			{Name: "context-pack", Args: "<path>", Summary: "compact a role-prefixed transcript file", Legacy: []string{"--context-pack-file"}},
		},
		Examples: []string{
			`atteler memory search "OAuth retry storm"`,
			`atteler memory retrieve "OAuth retry storm"`,
			`atteler memory retrieve "OAuth retry storm" --retrieval-source vector`,
			`atteler memory git-history "memory regression"`,
			`atteler memory vector-search "redirect risks"`,
			`atteler memory vector-index docs/research.md`,
		},
	},
	{
		Name:     codeIntelDomainName,
		Title:    "Code intelligence",
		Summary:  "Run Go code index, import graph, package/file/symbol, impact, and optional LSP queries without an LLM call; add --json or --output json for the stable schema.",
		Aliases:  []string{"code", "codeintel", "code-intelligence"},
		Commands: focusedCodeIntelDomainCommandAliases(),
		// Keep human help focused while accepting every descriptor-generated
		// query as a grouped command. The full generated contract is exposed
		// through `atteler config commands-docs`.
		RoutingCommands: codeIntelDomainCommandAliases(),
		Examples:        codeIntelDomainExamples(),
	},
	{
		Name:    "incident",
		Title:   "Incidents",
		Summary: "Diagnose production incidents from observability sources and prepare test-first fixes with redacted reports.",
		Aliases: []string{"incidents", "prod-incident"},
		Commands: []cliCommandAlias{
			{
				Name:    "diagnose",
				Summary: "fetch incident context, link stack traces to code, plan reproduction/tests/fix, and optionally prepare a PR body",
				Legacy:  []string{"--incident-diagnose"},
			},
		},
		Examples: []string{
			`atteler incident diagnose --sentry ISSUE-912`,
			`atteler incident diagnose --incident-file redacted-sentry-event.json`,
			`atteler incident diagnose --sentry ISSUE-912 --incident-apply-fix --incident-validation-command "go test ./pkg/auth"`,
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
		Summary: "Run sessions in isolated git worktrees; worktrees are preserved by default and merge-back is review-gated.",
		Aliases: []string{"worktree", "wt"},
		Commands: []cliCommandAlias{
			{Name: "run", Args: "[prompt]", Summary: "enable worktree isolation for this session; add --worktree-auto-merge plus --worktree-verify-command to merge on exit", Legacy: []string{"--worktree"}, JoinArgs: true},
			{Name: "list", Summary: "list active atteler worktrees", Legacy: []string{"--list-worktrees"}},
			{Name: "merge", Args: "<session-id>", Summary: "manually merge a session worktree back into its base branch; add --worktree-verify-command to run checks first or --merge-worktree-allow-base-mismatch to override the recorded-base preflight", Legacy: []string{"--merge-worktree"}},
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
		Summary: "Compare deterministic outputs with text or structured assertions, record/replay one-shot response fixtures, and record agent evaluations.",
		Aliases: []string{"evaluation", "evaluations"},
		Commands: []cliCommandAlias{
			{Name: "output", Args: "<path>", Summary: "compare actual output against expected text/file", Legacy: []string{"--eval-output"}},
			{Name: "run", Args: "<path>", Summary: "run a structured YAML/JSON assertion file", Legacy: []string{"--eval-assertions"}},
			{Name: "fixtures", Args: "<dir>", Summary: "discover and run structured eval fixtures in a directory", Legacy: []string{"--eval-fixture-dir"}},
			{Name: "list", Summary: "list agent evaluations on --session", Legacy: []string{"--list-evaluations"}},
			{Name: "record", Args: "<agent>", Summary: "append an evaluation to --session", Legacy: []string{"--record-evaluation"}},
			{Name: "record-response", Args: "<path> <prompt|--stdin>", Summary: "run one prompt and write request/response JSON", Legacy: []string{"--record-response"}, PromptAfterValue: true},
			{Name: "replay-response", Args: "<path> <prompt|--stdin>", Summary: "run one prompt from a recorded response JSON", Legacy: []string{"--replay-response"}, PromptAfterValue: true},
		},
		Examples: []string{
			`atteler eval output .atteler/fixtures/readme-summary.txt --eval-expected "package overview"`,
			`atteler eval run .atteler/evals/readme.eval.yaml`,
			`atteler eval fixtures .atteler/evals --eval-report .atteler/eval-report.json`,
			`atteler eval record reviewer`,
			`atteler eval replay-response .atteler/fixtures/once.json "Summarize @README.md"`,
		},
	},
}

var implicitFlagDefaults = map[string]string{
	"agent-memory-limit":          "5",
	"agent-memory-ttl-seconds":    "none",
	"bash-timeout-seconds":        "120",
	"context-pack-tokens":         "unlimited; capped by --model window when known",
	"git-history-limit":           "5",
	"mcp-timeout-seconds":         "none",
	"memory-limit":                "5",
	"memory-retention-days":       "disabled",
	"memory-ttl-seconds":          "none",
	"merge-artifact-max-bytes":    strconv.FormatInt(watch.DefaultLargeFileBytes, 10),
	"plan-max-agents":             "unlimited",
	"plugin-timeout-seconds":      "30",
	"prompt-complete-limit":       "5",
	"retrieval-limit":             "5",
	"skill-max-steps":             "6",
	"skill-min-occurrences":       "2",
	"spawn-cost-budget-micros":    "none",
	"spawn-ledger":                ".atteler/runs/.../ledger.json",
	"spawn-max-concurrency":       "4",
	"spawn-output-budget-bytes":   "none",
	"spawn-retries":               "0",
	"spawn-retry-backoff-seconds": "none",
	"spawn-task-timeout-seconds":  "none",
	"spawn-timeout-seconds":       "none",
	"spawn-token-budget":          "none",
	"vector-base-url":             vectorDefaultBaseURL,
	"vector-chunk-max-runes":      "1200",
	"vector-chunk-overlap-runes":  "120",
	"vector-fallback":             "fail",
	"vector-limit":                "5",
	"vector-model":                vectorDefaultModel,
	"vector-provider":             vectorDefaultProvider,
	"vector-store":                ".atteler/vector-index.json",
	"vector-timeout-seconds":      vectorDefaultTimeoutSeconds,
	"vectorizer":                  "lexical",
	"watch-interval-seconds":      "60",
	"watch-gate-min-severity":     "high",
	"watch-issue-min-severity":    "high",
	"watch-large-file-bytes":      strconv.FormatInt(watch.DefaultLargeFileBytes, 10),
	"watch-max-iterations":        "unlimited",
}
