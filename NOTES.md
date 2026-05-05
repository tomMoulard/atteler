# atteler

A go program that can be run using `go run ./cmd/atteler/` or `go run github.com/tommoulard/atteler/cmd/atteler@latest`.

It's a one of a kind LLM harness that leverage multiple LLMs. Here is the list of features in no particular order, on top of already exsting claude code features:

 - auto import configuration for other coding harnesses (e.g., claude code, codex, opencode, ...).
 - async all the way: all agents should run asynchronously and in parallel, and the system should be able to handle that (e.g., if an agent is waiting for a response from an LLM, it shouldn't block other agents from doing their work).
 - does feedback improvement of the defined agents. Once a task is completed, the system should do a retrospective of the execution and actually improve the agents that were involved in the execution of the task. For example, if an agent was not able to complete its task, the system should analyze why and improve the agent's prompt or tool usage accordingly. Keep an history of the changes made to the agents and the reasons for those changes. This way, the system can learn from its mistakes and improve over time.
 - agents can spawn other agents.
 - agents can have a memory (i.e., using a local vector database).
 - Speculative parallel execution: executing a task should spawn task exploration with multiple llm thinking at the same time, and doinging 3 rounds of confrontation before executing the task. For example: it can be run with any number of agents (this should be configurable):
   - first round: each agent thinks independently and come up with a plan to execute the task.
   - second round: agent A review the proposition of agent B to improve it's own proposition, and vice versa.
   - third round: have an agent C review all propositions and aggregate to a final proposition. the gate should be structured: tests pass, types pass, lint pass, no new flakes, behavioral diff vs baseline, optionally a property check. The judge agent should be a tiebreaker, not the primary signal. The user's input can be gathered to have a referee role in choosing the best trajectory.
 - Skill synthesis. When the agent does the same multi-step thing twice, it proposes turning it into a named skill (parameterized prompt + tool sequence) and asks if you want to keep it. The toolset grows with use. Compounds across the team if you commit them.
 - sandbox: all the work done by any agent must be usable: research plans, design decisions, ADR, code. The system should be able to surface that work in a way that is easily accessible and usable by the user. For example, if an agent does some research on a topic, the system should be able to surface that research in a way that the user can easily access it and use it for their own work. Or a code change can be aggregated from the work of two agents that ran side by side. An agent can be specialized in doing code merges
 - agent registry: a way to register agents with their capabilities, personality, and other metadata. This registry can be used to find the right agent for a given task, or to find agents that can work together on a task.
 - agent orchestration: a way to orchestrate the work of multiple agents on a given task. For example, if a task requires both research and coding, the system should be able to orchestrate the work of a research agent and a coding agent to complete the task.
 - agent evaluation: a way to evaluate the performance of agents on a given task. This can be done by comparing the output of the agent to a reference output, or by using a human evaluator to assess the quality of the agent's work. The system should be able to use this evaluation to improve the agents over time.
 - local rag of files, git history, and other relevant data sources to provide agents with the information they need to complete their tasks. This local rag should be fast and efficient, and should be able to handle large amounts of data. Should be synched async.
 - Cost & model routing as a first-class layer. This is literally your job. A multi-LLM harness without smart routing leaves the biggest win on the floor. Per-agent model preference with fallback chains, per-task budget caps that hard-stop runaway loops, prompt-cache reuse across speculative branches (huge if branches share prefix), TTFT-aware routing for interactive vs batch agents. Bake it in from day one — retrofitting later is painful.
 - Negative knowledge. Memory of what didn't work. "We tried X, broke Y, here's the commit." Cheap to capture, massive value — most current harnesses re-suggest the same broken approach forever.
 - Determinism knobs. Seed, temperature-0 mode, response recording/replay. Without these your eval and self-improvement loops are measuring noise.
 - modular: anyone can bring in code, tools, agents, and prompts from anywhere. The system should be able to integrate with any existing codebase, tool, or agent, and should be able to use them in a way that is seamless and efficient. For example, if there is an existing agent that does a specific task well, the system should be able to integrate that agent into its workflow without requiring a lot of work to adapt it to the system. kind of plugins systems.
 - SDK first: the cli tool is "just" an interface built on top of a powerful SDK that can be used to build custom workflows, agents, and tools. The SDK should be well-documented and easy to use, and should provide a lot of flexibility for users to build their own custom solutions on top of the system.
 - autocomplete the rest of the line (a la copilot) when writing a prompt, so you can easily reference tools, agents, and other resources in your prompts without having to remember their exact names or syntax. This should be available both in the CLI and in any other interfaces that the system provides.
 - have events hooks that can be used to trigger actions when certain events happen in the system. For example, when an agent completes a task, it can trigger an event that can be used to update the agent's performance metrics, or to trigger a feedback loop to improve the agent's performance on future tasks.

Here is a more in depth list of features:

 - Support for multiple LLMs (OpenAI, Anthropic, Cohere, etc.)
 - CLI tool
 - agents can have a personality (e.g., "be more concise", "be more verbose", "be more creative", "be more logical", etc.); a temperature; a reasoning level.
 - agents can either be called directly (@{agent_name}) or indirectly (e.g., "review this" and it calls the reviewer agent).
 - sessions + replay
 - fast to open
 - treesitter + LSP
 - Vector DB for prose/ADRs, graph for code.
 - MCPs
 - smart context compression (i.e., the context that is sent to the llm can be compressed, but the full context should be kept in case some data gets distilled).
 - reference things using the `@` sign with auto complete.

## Agents ideas

### 1- Continuous background agent.

A separate loop that watches the repo independently of any active session — flagging perf regressions, dead code, missing tests, drift from conventions. Like a local CI that thinks. Surfaces work proactively rather than waiting for you to ask.

### 2- Review agent

Should work in the same maner as the Speculative parallel execution, but for code review.
I should/can also include specialized tool to do the review like coderabbit.

## TODO

 - [x] llm connection (claude code, codex)
 - [x] Claude Code provider exposes Bash for shell-command requests
 - [x] local Ollama provider for offline/local inference
 - [x] auto-start local Ollama daemon for selected local Ollama runs
 - [x] machine-readable JSON output for watch scans and hook event inventory
 - [x] offline doctor/readiness inventory that avoids provider health checks
 - [x] configuration loading
    - [x] general configuration
    - [x] local configuration
    - [x] other harness configurations
        - [x] codex
        - [x] claude code
        - [x] opencode
    - [x] YAML config template/init command
    - [x] config path listing and validation
 - [x] sessions + replay
 - [x] CLI compact session message listing
 - [x] session titles
 - [x] session tags
 - [x] session tag summaries
 - [x] session discovery/listing
 - [x] YAML session details command
 - [x] compact session summary command
 - [x] markdown/json session export
 - [x] transcript search
 - [x] config-backed agent registry and `@agent` invocation
 - [x] config-backed agent reasoning-level metadata
 - [x] CLI reasoning-level override
 - [x] response token usage summaries with cached-input accounting
 - [x] YAML agent description command
 - [x] local readiness doctor
 - [x] build-time version reporting
 - [x] pipe-friendly stdin one-shot prompts
 - [x] local event hooks
 - [x] bounded local `@file` context references
 - [x] bounded local `@directory` tree references
 - [x] trigger-based indirect agent routing
 - [x] agent metadata, capabilities, and capability-backed prompt matching
 - [x] global and per-agent model fallback chains
 - [x] global/agent/CLI generation controls
 - [x] determinism seed knob and request input-token guardrails
 - [x] local plugin manifest discovery/validation
 - [x] local plugin entrypoint execution helper for SDK workflows
 - [x] negative-knowledge capture, search, show, and export
 - [x] CLI session negative-knowledge inventory listing
 - [x] agent evaluation capture, search, show, and export
 - [x] CLI session evaluation inventory listing
 - [x] aggregate agent performance summaries across sessions
 - [x] sandbox artifact manifest capture, search, show, and export
 - [x] CLI session artifact inventory listing
 - [x] deterministic response recording/replay fixtures
 - [x] dependency-free agent orchestration planning
 - [x] CLI agent orchestration preview
 - [x] dependency-aware async agent task planning waves
 - [x] dependency-aware async agent task runner with same-wave concurrency
 - [x] persistent agent task/TODO list with add, assign, complete, and list commands
 - [x] CLI dependency-aware async task planning preview
 - [x] agent feedback improvement proposal primitives
 - [x] CLI feedback improvement proposal report
 - [x] cost/model routing primitives with budget, context, cache, and latency signals
 - [x] CLI cost/model routing preview
 - [x] smart context compression primitives with omission accounting
 - [x] CLI smart context compression preview
 - [x] MCP manifest validation and capability lookup primitives
 - [x] CLI MCP manifest validation and capability lookup
 - [x] MCP stdio JSON-RPC client invocation primitive and CLI tool/method call
 - [x] dependency-free evaluation helpers for agent outputs
 - [x] CLI eval check runner
 - [x] dependency-free local memory/RAG lexical index
 - [x] CLI memory indexing/search over files and saved sessions
 - [x] CLI git history lexical search for local RAG
 - [x] CLI local vector search over indexed files
 - [x] CLI plugin describe, dry-run, and entrypoint execution
 - [x] skill synthesis suggestion primitive and CLI
 - [x] skill acceptance and markdown persistence
 - [x] interactive `@` completion for agents and local paths
 - [x] deterministic rest-of-line prompt completion primitive and CLI preview
 - [x] LSP document-symbol code intelligence primitive and CLI
 - [x] dependency-free Go code intelligence and import graph foundation
 - [x] CLI Go code index and graph summary
 - [x] CLI Go file inventory with package/import/symbol counts
 - [x] CLI Go package inventory with file and symbol counts
 - [x] CLI Go package file inventory
 - [x] CLI Go package import usage summary
 - [x] CLI Go package import-count summary
 - [x] CLI Go package exact import usage summary
 - [x] CLI Go package exact import file listing
 - [x] CLI Go package exact import file-count summary
 - [x] CLI Go package import file-count summary
 - [x] CLI Go package import-prefix file listing
 - [x] CLI Go package import-prefix file-count summary
 - [x] CLI Go package import-prefix usage summary
 - [x] CLI Go package symbol kind summary
 - [x] CLI Go package symbol file-count summary
 - [x] CLI Go package symbol listing
 - [x] CLI Go package exact symbol filtering
 - [x] CLI Go package exact symbol-name file-count summary
 - [x] CLI Go package symbol kind filtering
 - [x] CLI Go package symbol-kind file-count summary
 - [x] CLI Go package symbol prefix filtering
 - [x] CLI Go package symbol-prefix file-count summary
 - [x] CLI Go file import and symbol inventory
 - [x] CLI Go file import listing
 - [x] CLI Go file symbol listing
 - [x] CLI Go file symbol kind summary
 - [x] CLI Go file exact import lookup
 - [x] CLI Go file import prefix filtering
 - [x] CLI Go file symbol kind filtering
 - [x] CLI Go file exact symbol filtering
 - [x] CLI Go file symbol prefix filtering
 - [x] CLI Go symbol lookup over the local repository
 - [x] CLI Go exact symbol-name file-count summary
 - [x] CLI Go exact symbol-name package-count summary
 - [x] CLI Go symbol prefix lookup over the local repository
 - [x] CLI Go symbol-prefix file-count summary
 - [x] CLI Go symbol-prefix package-count summary
 - [x] CLI Go symbol kind lookup over the local repository
 - [x] CLI Go symbol-kind file-count summary
 - [x] CLI Go symbol-kind package-count summary
 - [x] CLI Go symbol kind summary
 - [x] CLI Go symbol file-count summary
 - [x] CLI Go import-edge listing over the local repository
 - [x] CLI Go import usage summary
 - [x] CLI Go import file-count summary
 - [x] CLI Go import-path usage lookup
 - [x] CLI Go import-path usage summary
 - [x] CLI Go import-path file-count summary
 - [x] CLI Go import-path package-count summary
 - [x] CLI Go import-prefix usage lookup
 - [x] CLI Go import-prefix usage summary
 - [x] CLI Go import-prefix file-count summary
 - [x] CLI Go import-prefix package-count summary
 - [x] CLI Go import graph topological layers
 - [x] CLI Go import graph cycle detection
 - [x] CLI Go import impact lookup over the local repository
 - [x] CLI Go import graph reachability lookup over the local repository
 - [x] dependency-free code graph traversal and impact analysis primitives
 - [x] CLI direct Go import graph dependency and reverse-dependency lookup
 - [x] dependency-free vector retrieval primitive
 - [x] per-agent persistent vector memory primitive
 - [x] CLI per-agent vector memory indexing/search
 - [x] speculative three-round execution planning primitives
 - [x] speculative three-round session runner primitives
 - [x] speculative prompt-cache shared-prefix estimate primitives
 - [x] CLI speculative three-round execution plan preview
 - [x] CLI speculative prompt-cache reuse preview
 - [x] structured review-agent report and gate-check primitives
 - [x] review-agent speculative plan preview
 - [x] CLI structured review scan report
 - [x] continuous background-agent repository scan primitives
 - [x] CLI background-agent repository scan
 - [x] CLI continuous background-agent watch loop
 - [x] background convention-drift scan for misplaced `context.Background()`
 - [x] explicit local bash command runner
 - [x] concurrent sub-agent spawning primitive and CLI dry-run/runner
 - [x] CLI feedback proposal application to config and history log
 - [x] CLI model-route budget hard-stop for one-shot/stdin requests
 - [x] sandbox artifact merge aggregation primitive
 - [x] CLI merged artifact markdown export
 - [x] context-aware command propagation with a single main entry context
 - [x] providerless local inspection commands avoid credential/network side effects
 - [x] `DEBUG_ATTELER_*` environment aliases for local debug/inspection flags
 - [x] LSP workspace-symbol lookup primitive and CLI
 - [x] CLI hook-event discovery
 - [x] CLI session inventory filtering by exact tag
 - [x] offline built-in provider/model listing
 - [x] black-box CLI e2e tests for common config, provider, agent, and session workflows
 - [x] opt-in live LLM e2e tests for OpenAI and Anthropic one-shot calls
