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
  There is no `--help` grouping, no subcommands, no man page. With 220 flags,
  the flat `--help` output is unusable. Group flags by domain (provider, session,
  agent, code-intel, memory, etc.) in the help output. Consider generating a man
  page or adopting a subcommand structure for the inspection/utility commands.
  Also, add the default values to the help output for better discoverability.

- [x] **Upgrade the vector store for real-world RAG.**
  `pkg/vector/vector.go` uses lexical feature hashing (FNV on bigrams into 128
  dimensions) with cosine similarity. This is fine for tiny personal stores but
  search quality degrades quickly. Consider optional integration with a real
  embedding model (local via Ollama, or an API) and/or an ANN index for
  sub-linear search. Keep the current implementation as the zero-dependency
  fallback.

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
