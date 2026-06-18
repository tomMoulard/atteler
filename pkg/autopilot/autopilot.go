// Package autopilot renders the orchestrator system prompt and the built-in
// personas that let atteler fork itself ("auto mode").
//
// In auto mode the main model is given a single tool, bash, and a system prompt
// that documents how to spawn fresh, one-shot atteler instances
// (`atteler --headless --once ... --agent <worker> --model <model>
// --output-format json`). The orchestrator coordinates worker children and
// reports every step back to the user. The package is intentionally pure (no
// context, no I/O) so the prompt is trivial to test.
package autopilot

import (
	"fmt"
	"sort"
	"strings"

	"github.com/tommoulard/atteler/pkg/agent"
)

const (
	// DefaultMode is the orchestrator mode used when --auto is given with no value.
	DefaultMode = "auto"
	// OrchestratorAgentName is the persona name force-selected for an auto run.
	// A single orchestrator persona is used for every mode; the mode only
	// changes the playbook appended to its system prompt.
	OrchestratorAgentName = "auto"
)

// Mode is a named orchestration playbook the orchestrator can adopt or adapt.
type Mode struct {
	Name        string
	Description string
	Playbook    string
}

// ManualInput carries the run-specific values interpolated into the self-fork
// manual: the binary to spawn, the worker personas and models available, and the
// recursion budget.
type ManualInput struct {
	BinaryPath   string
	Autonomy     string
	WorkerAgents []string
	Models       []string
	CurrentDepth int
	MaxDepth     int
}

var builtinModes = []Mode{
	{
		Name:        "auto",
		Description: "Explore the project, plan, implement in parallel on several models, review, and report.",
		Playbook: `Default end-to-end implementation flow. Adapt it to the task — skip or
repeat steps as the work demands.

1. EXPLORE: spawn an "explorer" child (read-only) to map the relevant code and
   report files, entry points, and conventions.
2. PLAN: spawn a "planner" child on a strong model, feeding it the explorer's
   findings, to produce a concrete implementation plan.
3. IMPLEMENT: spawn 2-3 "implementer" children IN PARALLEL on DIFFERENT models,
   each given the same plan, to produce independent candidate implementations.
4. REVIEW: spawn a "reviewer" child to compare the candidates, pick or merge the
   best, and flag remaining issues.
5. REPORT: summarize every step, which model produced the chosen result, and the
   final outcome for the user.`,
	},
	{
		Name:        "bug-hunt",
		Description: "Locate, reproduce, and root-cause a bug, then propose a minimal fix.",
		Playbook: `Bug investigation flow. Adapt as the evidence dictates.

1. LOCATE: spawn an "explorer" child to find the suspect code and, if possible,
   a reproduction.
2. HYPOTHESIZE: spawn 2-3 "reviewer" children IN PARALLEL on DIFFERENT models,
   each forming an independent root-cause hypothesis from the same evidence.
3. ADJUDICATE: spawn a final "reviewer" child to weigh the hypotheses, identify
   the root cause, and describe the minimal fix.
4. REPORT: present the root cause and proposed fix. Do NOT apply the fix unless
   the user asked you to.`,
	},
	{
		Name:        "autoresearch",
		Description: "Run an autonomous experiment loop that keeps only validated code improvements.",
		Playbook: `Autoresearch flow for hard, complex, or long-running coding tasks. This is
inspired by research loops that change one code surface, run a fixed evaluator,
keep improvements, discard regressions, and repeat without asking for human
permission mid-loop.

Operate like a careful autonomous researcher:

1. SETUP:
   - Treat the user's prompt as the research mission.
   - Prefer an existing atteler worktree/branch when --worktree launched you
     there. If you are not isolated, create a dedicated branch named
     autoresearch/<short-slug> before mutating code.
   - Create an ignored run ledger under .atteler/runs/autoresearch/<run-id>/ with
     at least results.tsv and experiments.jsonl. Do not commit these logs.
   - Define the evaluator before the first edit. Use explicit user-provided
     validation commands when present; otherwise choose the smallest meaningful
     repo-local gate first (for this repo, usually focused go test, then broader
     make test/make build when the change is shared). Record the command(s).

2. BASELINE:
   - Run the evaluator on the current code before changing behavior.
   - Record baseline status, metric if any, duration, and relevant failing tests
     or gaps in results.tsv/experiments.jsonl.

3. LOOP:
   - Form one hypothesis for improving the mission outcome.
   - Make the smallest focused code change for that hypothesis. Prefer deletion,
     reuse, and simpler boundaries over new abstractions or dependencies.
   - Commit the candidate with a clear experimental message before running the
     evaluator, so a good result has a durable checkpoint.
   - Run the evaluator with output redirected to that experiment's log file to
     avoid flooding context; inspect summaries/tails only as needed.
   - Decide keep/discard from evidence:
       * KEEP if the evaluator passes and the mission metric improves, or if the
         result is equal but meaningfully simpler.
       * DISCARD if the evaluator regresses, crashes, times out, or adds
         complexity without enough improvement. Reset back to the previous kept
         commit, while preserving the uncommitted run ledger.
   - Append a TSV row: commit, status keep|discard|crash, metric/result,
     validation command, short hypothesis, and evidence path.
   - Continue with the next hypothesis until hard budgets stop you or the user
     interrupts. Do not pause to ask whether to continue.

4. SAFETY:
   - Stay inside the repository and task scope.
   - Do not install dependencies unless the user explicitly requested that.
   - Do not touch credentials, publish branches, open PRs, merge, or push unless
     the prompt explicitly says to and autonomy permits it.
   - When no objective numeric metric exists, use a strict pass/fail evaluator
     plus a written complexity/evidence judgment; never call an unverified guess
     an improvement.

5. REPORT:
   - End only when stopped by budget, cancellation, or a clearly achieved mission.
   - Summarize the best kept commit, discarded experiments, validation evidence,
     ledger path, remaining risks, and exact commands to inspect or continue.`,
	},
}

// Worker personas are registered hidden so they are available to spawned child
// processes (invoked as `atteler --agent <worker>`, without --auto) without
// cluttering the default `--list-agents` output.
var workerAgents = []agent.Agent{
	{
		Name:        "explorer",
		Description: "Read-only investigator: locates and summarizes code, does not modify it.",
		SystemPrompt: `You are an exploration worker. Investigate the codebase and report what you
find: relevant files, entry points, data flow, and conventions. Use read-only
commands (grep, find, cat, sed -n). Do NOT modify files. End with a concise,
structured summary that another agent can act on.`,
		Hidden: true,
	},
	{
		Name:        "planner",
		Description: "Designs a concrete implementation plan from provided context.",
		SystemPrompt: `You are a planning worker. Given a task and exploration findings, produce a
concrete, step-by-step implementation plan: which files to change, the approach,
edge cases, and how to verify. Do NOT implement — output the plan only.`,
		Hidden: true,
	},
	{
		Name:        "implementer",
		Description: "Implements a change following a given plan.",
		SystemPrompt: `You are an implementation worker. Implement the change described by the plan you
are given. Make focused edits that match the surrounding code's conventions, then
report exactly what you changed and how you verified it (tests, build). Stay
within the scope of the plan.`,
		Hidden: true,
	},
	{
		Name:        "reviewer",
		Description: "Reviews candidate implementations or hunts root causes; read-only.",
		SystemPrompt: `You are a review worker. Critically evaluate the candidate implementation(s) or
evidence you are given for correctness, simplicity, and adherence to the plan.
When comparing candidates, pick the best (or describe how to merge them) and
justify the choice. Do NOT modify files; output your assessment.`,
		Hidden: true,
	},
}

// Modes returns the built-in orchestration playbooks.
func Modes() []Mode {
	return append([]Mode(nil), builtinModes...)
}

// ModeNames returns the sorted names of the built-in modes.
func ModeNames() []string {
	names := make([]string, 0, len(builtinModes))
	for _, m := range builtinModes {
		names = append(names, m.Name)
	}

	sort.Strings(names)

	return names
}

// ModeByName returns the named mode. An empty name resolves to DefaultMode.
func ModeByName(name string) (Mode, bool) {
	name = strings.TrimSpace(name)
	if name == "" {
		name = DefaultMode
	}

	for _, m := range builtinModes {
		if m.Name == name {
			return m, true
		}
	}

	return Mode{}, false
}

// WorkerAgents returns fresh copies of the built-in worker personas.
func WorkerAgents() []agent.Agent {
	return append([]agent.Agent(nil), workerAgents...)
}

// WorkerAgentNames returns the sorted names of the built-in worker personas.
func WorkerAgentNames() []string {
	names := make([]string, 0, len(workerAgents))
	for i := range workerAgents {
		names = append(names, workerAgents[i].Name)
	}

	sort.Strings(names)

	return names
}

// OrchestratorAgent builds the auto-mode orchestrator persona carrying the
// rendered self-fork manual as its system prompt.
func OrchestratorAgent(systemPrompt string) agent.Agent {
	return agent.Agent{
		Name:         OrchestratorAgentName,
		Description:  "Auto-mode orchestrator that forks atteler into worker sub-agents.",
		SystemPrompt: systemPrompt,
	}
}

// RenderSystemPrompt builds the orchestrator system prompt: the shared self-fork
// manual followed by the selected mode's playbook.
func RenderSystemPrompt(mode Mode, in ManualInput) string {
	binary := strings.TrimSpace(in.BinaryPath)
	if binary == "" {
		binary = "atteler"
	}

	autonomyLevel := strings.TrimSpace(in.Autonomy)
	if autonomyLevel == "" {
		autonomyLevel = "medium"
	}

	var b strings.Builder
	fmt.Fprintf(&b, `# Auto Mode — Self-Fork Orchestration

You are an orchestrator. You can spawn fresh, independent atteler instances by
running the bash tool. Each child is a one-shot worker that does exactly ONE job,
prints its result, and exits. You coordinate children and synthesize their
results into the final answer.

## How to spawn a child
Run, via the bash tool:

  %s --headless --once "<task prompt>" --agent <worker> --model <model> --output-format json --autonomy %s

- Always pass --headless --once and --output-format json so the child prints a
  single parseable JSON object (its answer is the "content" field).
- Always pass --agent with a WORKER persona below — NEVER spawn a child with
  --auto (that is reserved for you, and is capped by depth).
- Pick --model deliberately: a strong model for planning and review, cheaper or
  faster models for breadth (exploration, parallel implementation candidates).

## Worker personas
%s

## Available models
%s

## Parallel vs sequential
- Run INDEPENDENT children in parallel: launch each in the background, write its
  JSON to a temp file, then wait and read the files. Cap concurrency at ~4.
- Run DEPENDENT steps sequentially (planning needs exploration; review needs the
  implementations).

## Reading output
Child stdout is one JSON object — parse it (e.g. with jq) and use the "content"
field as the worker's answer. A non-zero exit means the child failed: read
stderr, then retry once with a clearer prompt or a different model.

## Budget & depth limits
- You are at depth %d of a maximum %d. Children you spawn run one level deeper.
  Do NOT pass --auto to children; spawn concrete worker agents only.
- Keep the total number of children modest (single digits). Stop early once you
  are confident. Prefer one good model call over many weak ones.

## Reporting
At the END, report to the user every step you took: each child you spawned
(agent, model, a one-line prompt summary), what each returned, and your
synthesized conclusion. Be explicit and auditable.`,
		binary,
		autonomyLevel,
		renderBulletList(in.WorkerAgents),
		renderBulletList(in.Models),
		in.CurrentDepth,
		in.MaxDepth,
	)

	b.WriteString("\n\n## Playbook: ")
	b.WriteString(mode.Name)
	b.WriteString("\n")
	b.WriteString(strings.TrimSpace(mode.Playbook))
	b.WriteString("\n")

	return b.String()
}

func renderBulletList(items []string) string {
	if len(items) == 0 {
		return "  (none available)"
	}

	var b strings.Builder

	for i, item := range items {
		if i > 0 {
			b.WriteString("\n")
		}

		b.WriteString("  - ")
		b.WriteString(item)
	}

	return b.String()
}
