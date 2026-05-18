# atteler TODO

## P0 -- Broken (CI is red, legal blockers)

- [ ] **Fix `stripANSI` test helper bug.**
  `cmd/atteler/main_test.go:1423-1443` -- the ANSI-stripping function fails to
  advance past the `[` byte in CSI sequences, so parameter bytes like `37m` leak
  into stripped output as literal text. `TestView_RendersInlinePromptSuggestion`
  is failing because of this. Either insert the missing `i++` after the `[`
  check, or replace the hand-rolled parser with `charmbracelet/x/ansi.Strip()`
  which is already a transitive dependency. Audit every other test that calls
  `stripANSI` to confirm they still pass after the fix.

## P1 -- Maintainability (structural debt)

- [ ] **Split `cmd/atteler/main.go` into separate files.**
  The file is 11,856 lines with 483 functions mixing at least seven unrelated
  concerns. Suggested split:
  - `tui.go` -- the Bubble Tea `model` struct, `Update`, `View`, helpers.
  - `picker.go` -- model picker, fzf integration, `loadModels`, scope picker.
  - `codeintel_commands.go` -- the ~4,500 lines of `--code-*` subcommands that
    are pure transformations on `codeintel.Index` with zero TUI or LLM coupling.
  - `session_commands.go` -- session list/show/export/search/replay commands.
  - `dispatch.go` -- `parseOptions`, `cliOptions`, the cascading
    `(bool, error)` handler chain, debug env overlays.
  Keep `main.go` as a thin `main()` + `run()` that wires things together.
  All code stays in `package main`; this is a file split, not a refactor.

- [ ] **Replace the cascading dispatch chain with a command registry.**
  The ~15-deep chain of `(bool, error)` handler functions
  (`runProviderlessCommand`, `runStateCodeSymbolCommand`, etc.) is hard to
  navigate and extend. Consider a flat `map[string]handlerFunc` keyed by the
  primary flag name, or adopt a lightweight subcommand library. The goal is that
  adding a new CLI flag no longer requires touching five dispatch functions.

## P2 -- Reliability (production readiness gaps)

- [ ] **Add HTTP client timeouts to all LLM providers.**
  Every provider creates `&http.Client{}` with no `Timeout`. A hung provider
  blocks until context cancellation (if the caller even passes a deadline). Add a
  reasonable default timeout (e.g., 120s) at construction time. Ideally make it
  configurable per-provider in the YAML config. Affected locations:
  `pkg/llm/openai.go:42`, `pkg/llm/anthropic.go:58`, `pkg/llm/claude_code.go:45`,
  `pkg/llm/codex.go:49`, `pkg/llm/ollama.go:51`.

- [ ] **Add retry with backoff for transient provider errors.**
  None of the five providers retry on 429 (rate limit), 503, or 5xx responses.
  `CompleteWithFallback` tries the next fallback model but never retries the same
  model. Add a small retry loop (2-3 attempts, exponential backoff, respect
  `Retry-After` headers) inside or around `Complete()`. Keep it opt-out so tests
  stay fast.

- [ ] **Add streaming support to the `Provider` interface.**
  All `Complete()` calls wait for the full response before returning. For long
  answers in the interactive TUI the user sees nothing until the response is
  done. Design a `CompleteStream(ctx, params) (<-chan Chunk, error)` or
  `io.Reader`-based API. Wire it into the TUI so tokens render as they arrive.
  The non-streaming `Complete()` can remain for one-shot and headless callers.

- [ ] **Add SIGTERM/SIGINT graceful shutdown.**
  The TUI catches `Ctrl+C` via `handleCtrlC` and calls `m.cancel()`, but session
  save runs as a `tea.Cmd` that may not complete if the process exits. Register
  an OS signal handler that saves the session synchronously before exiting.
  Background goroutines (watch loop, headless runs) should also drain cleanly.

- [ ] **Add config schema validation with helpful errors.**
  The YAML config loader silently ignores unknown fields. A typo like
  `defualt_provider` produces a confusing downstream error instead of a clear
  "unknown field" message at load time. Use `yaml.Decoder` with
  `KnownFields(true)` or a post-load validation pass. Offer "did you mean X?"
  suggestions for close matches.

## P3 -- Code quality and correctness

- [ ] **Add concurrency protection to `vector.Store`.**
  `pkg/vector/vector.go` -- the `Documents` slice has no synchronization. If two
  agents index and search concurrently (the stated use case for agent memory),
  this is a data race. Add a `sync.RWMutex` around `Add` and `Search`.

- [ ] **Remove the `nonNilContext()` safety net.**
  `cmd/atteler/main.go:3217-3223` silently replaces nil contexts with a
  background context, masking programming errors. In Go a nil context is always a
  bug. Either panic, return an error, or (safest) remove the helper and let the
  standard library panic surface the real call site.

- [ ] **Replace global `log.SetOutput` mutation with injected loggers.**
  `cmd/atteler/main.go:4257-4262` suppresses provider registration noise by
  mutating the global `log` default logger. This is thread-unsafe and violates
  the no-global-state guideline. Instead, pass a `*log.Logger` or `*slog.Logger`
  into `AutoRegisterWithConfigContext` and provider factories so callers control
  output without global side effects.

- [ ] **Bound prompt history loading at startup.**
  `cmd/atteler/main.go:1994-2029` loads messages from every saved session to
  populate the 100-entry history ring. For users with many sessions this is slow.
  Limit the scan to the N most recent sessions (by file mtime or session
  timestamp) or cache the history index.

- [ ] **Cancel model-picker API fetches when the picker is dismissed.**
  `cmd/atteler/main.go:1573-1594` fires concurrent `FetchModels` calls to all
  providers. Dismissing the picker discards results but does not cancel the
  in-flight HTTP requests. Give the picker its own `context.WithCancel` derived
  from `m.ctx` and cancel it on close.

## P4 -- Feature gaps (depth over breadth)

- [ ] **Wire up end-to-end speculative execution.**
  The three-round speculative runner exists in `pkg/speculate` and the CLI
  exposes `--speculate-plan`, but there is no integrated workflow that actually
  calls providers for proposal/cross-review/verdict rounds and presents the
  aggregated result. Build the full pipeline: plan, execute three rounds with
  real LLM calls, gate-check, present verdict.

- [ ] **Add multi-turn agent conversations with tool use.**
  The current architecture sends a flat `[]Message` in a single `Complete()`
  call. Agents cannot do iterative tool use (call a tool, observe the result,
  call again) within a single task. Design an agentic loop that supports function
  calling / tool-use message sequences. This is a prerequisite for agents that
  can actually edit code, run tests, and iterate.

- [ ] **Add structured logging.**
  Replace `log.Printf` and `fmt.Fprintf(os.Stderr, ...)` with `log/slog` or a
  similar structured logger. Add log levels (debug, info, warn, error),
  correlation IDs for sessions and agent runs, and JSON output mode. This is
  essential for debugging concurrent multi-agent runs.

- [ ] **Improve CLI discoverability for 220+ flags.**
  There is no `--help` grouping, no subcommands, no man page. With 220 flags,
  the flat `--help` output is unusable. Group flags by domain (provider, session,
  agent, code-intel, memory, etc.) in the help output. Consider generating a man
  page or adopting a subcommand structure for the inspection/utility commands.

- [ ] **Upgrade the vector store for real-world RAG.**
  `pkg/vector/vector.go` uses lexical feature hashing (FNV on bigrams into 128
  dimensions) with cosine similarity. This is fine for tiny personal stores but
  search quality degrades quickly. Consider optional integration with a real
  embedding model (local via Ollama, or an API) and/or an ANN index for
  sub-linear search. Keep the current implementation as the zero-dependency
  fallback.

## P5 -- Cleanup

- [ ] **Delete `NOTES.md`.**
  `NOTES.md` is a near-duplicate of the features section in `README.md`.
  Everything in it is already covered (and more current) in the README. Remove it
  to avoid confusion about which file is canonical.

- [ ] **Clean up old deadlock trace from this file.**
  The previous `TODO.md` included a pasted deadlock stacktrace from a bug that
  was fixed. It is now removed. Keep this file focused on actionable items only.

## Misc
- [ ] When executing a command (with `!<command>`):
    - [ ] the command can take over the shell (e.g., you should be able to run atteler inside atteler, or vim inside atteler, etc.) -> add integration test.
    - [ ] the output of the command should be both visiblie in the TUI, the logs, and the llm context.
    - [ ] There should be a sensible default value for bash-timeout-seconds. When reaching this limit, the llm should be notified that the command timed out, and either it should launch another command to resolve the timeout (e.g., launch a smaller sections of the tests), launch another command to debug the issue, or solve the issue whatsoever it thinks is best.
