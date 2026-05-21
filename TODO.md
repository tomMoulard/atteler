# atteler TODO

## P4 -- Feature gaps (depth over breadth)

- [x] **Wire up end-to-end speculative execution.**
  The three-round speculative runner exists in `pkg/speculate` and the CLI
  exposes `--speculate-plan`, but there is no integrated workflow that actually
  calls providers for proposal/cross-review/verdict rounds and presents the
  aggregated result. Build the full pipeline: plan, execute three rounds with
  real LLM calls, gate-check, present verdict.

- [x] **Add structured logging.**
  Replace `log.Printf` and `fmt.Fprintf(os.Stderr, ...)` with `log/slog`. Add
log levels (debug, info, warn, error), correlation IDs for sessions and agent
runs, and JSON output mode. This is essential for debugging concurrent
multi-agent runs.

- [x] **Improve CLI discoverability for 220+ flags.**
  The top-level `--help` output now stays short and points users at grouped
  domains (`atteler help <domain>`), while `atteler help legacy` keeps the full
  compatibility flag catalog with defaults for scripts that still use flat
  flags. No existing script-facing flag is deprecated in this release.
  Inspection and utility commands have grouped aliases across chat/session,
  config, providers, agents, memory/RAG, code-intel, review, watch, plugins,
  worktrees, and eval.

- [x] **Upgrade the vector store for real-world RAG.**
  `pkg/vector/vector.go` uses lexical feature hashing (FNV on bigrams into 128
  dimensions) with cosine similarity. This is fine for tiny personal stores but
  search quality degrades quickly. Consider optional integration with a real
  embedding model (local via Ollama, or an API) and/or an ANN index for
  sub-linear search. Keep the current implementation as the zero-dependency
  fallback.

- [x] **Wire up end-to-end review-agent execution.**
  The review-agent plan and structured scan primitives now have a full
  three-round LLM-backed workflow: independent reviews, cross-review of findings,
  aggregate verdict, and required gate checks surfaced through `--review-run`.

## Misc -- Shell execution improvements

- [x] **Interactive shell takeover for `!<command>`.**
  The `!<command>` handler should support interactive programs (e.g., vim,
  atteler-in-atteler). Add an integration test verifying PTY passthrough.

- [x] **Shell output visible in TUI, logs, and LLM context.**
  Command output from `!<command>` should be captured and displayed in the TUI,
  written to the log file, and appended to the LLM conversation context so the
  model can reason about it.

- [x] **Bash timeout with LLM-driven recovery.**
  Add a sensible default for `bash-timeout-seconds`. When a command times out,
  notify the LLM so it can decide to retry with a smaller scope, debug the
  issue, or take corrective action autonomously.

- [x] hook when executing the command (with the command input and command iteself), and hook with the command output
- [x] on exit, it should print the command to run to reuse the last session (e.g., `$0 --session-id <session-id>`).
- [x] add rtk (https://github.com/rtk-ai/rtk) integration/plugin
- [x] change the title of the terminal to add a spinner when the harness is doing work, and stop it when it waits for user input. This is a nice-to-have UX improvement to give feedback that the system is working on something.
- [x] in the TUI, when no input as been set for 1s, do a call to the llm provider to get a suggestion to complete the input. This is a nice-to-have feature to give suggestions to the user and help them complete their thoughts.
- [x] in the TUI, pressing the up/down arrow keys should cycle through the input history. But if the cursor is in the middle of the input, it should not cyrl e through the history, but instead move the cursor to the beginning/end of the input. This is a nice-to-have UX improvement to make it easier to navigate the input history.
- [x] in the TUI, when selecting an effort process, it should be stored and reused across sessions
