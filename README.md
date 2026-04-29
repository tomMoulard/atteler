# atteler

A multi-LLM harness built in Go that orchestrates multiple AI agents to work together on complex tasks.

## Overview

atteler is a CLI tool that leverages multiple LLMs (OpenAI, Anthropic, Cohere, etc.) through an agent-based architecture. It goes beyond single-model interaction by enabling parallel, asynchronous agent execution with built-in self-improvement, speculative planning, and intelligent model routing.

## Key Features

- **Multi-LLM Support** -- Route tasks to OpenAI, Anthropic, Cohere, and others with per-agent model preferences and fallback chains.
- **Async & Parallel Execution** -- All agents run asynchronously; no agent blocks another while waiting for an LLM response.
- **Speculative Parallel Planning** -- Spawn multiple agents to independently plan a task, cross-review each other's proposals, then aggregate into a final plan before execution.
- **Agent Registry & Orchestration** -- Register agents with capabilities, personality, temperature, and reasoning level. Orchestrate multi-agent workflows for tasks that span research, coding, and review.
- **Self-Improving Agents** -- After each task, a retrospective evaluates agent performance and refines prompts and tool usage. History of changes is tracked so the system learns over time.
- **Negative Knowledge** -- Remembers what didn't work to avoid repeating the same mistakes.
- **Skill Synthesis** -- When a multi-step pattern is repeated, it is proposed as a reusable named skill (parameterized prompt + tool sequence).
- **Local RAG** -- Vector DB for prose/ADRs and a graph for code, backed by files, git history, and other data sources. Synced asynchronously.
- **Sandbox & Surfacing** -- All agent work (research plans, design decisions, ADRs, code) is preserved and surfaceable. Code changes from parallel agents can be merged.
- **Smart Context Compression** -- Context sent to an LLM is compressed while the full context is retained for later retrieval.
- **Cost & Budget Controls** -- Per-task budget caps, prompt-cache reuse across speculative branches, and TTFT-aware routing.
- **Determinism Knobs** -- Seed, temperature-0 mode, and response recording/replay for reproducible evaluations.
- **Config Auto-Import** -- Import configurations from other coding harnesses (Claude Code, Codex, OpenCode, etc.).
- **Sessions & Replay** -- Persist and replay sessions for debugging and evaluation.
- **Tree-sitter & LSP** -- Structural code understanding via tree-sitter and LSP integration.
- **MCP Support** -- Model Context Protocol integration.

## Agent Examples

| Agent | Description |
|---|---|
| **Background Agent** | Watches the repo continuously, flagging perf regressions, dead code, missing tests, and convention drift. |
| **Review Agent** | Performs speculative parallel code review, optionally backed by specialized tools like CodeRabbit. |

## Getting Started

### Prerequisites

- Go 1.26+

### Install & Run

```bash
# Run from source
go run ./cmd/atteler/

# Or install globally
go install github.com/tommoulard/atteler/cmd/atteler@latest
```

### Using Make

```bash
make build    # Compile the binary
make run      # Build and run
make test     # Run tests
make lint     # Run linters
make generate # Run go generate
```

## License

See [LICENSE](LICENSE) for details.
