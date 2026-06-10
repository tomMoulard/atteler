# Repository Guidelines

## Project Structure & Module Organization

This repository is a Go module for the `atteler` CLI and supporting LLM package.

- `cmd/atteler/`: executable Bubble Tea TUI entrypoint.
- `pkg/llm/`: provider-agnostic LLM interfaces, registry, auth, and provider implementations.
- `pkg/llm/*_test.go`: unit tests for provider and registry behavior.
- `README.md`: product overview, quickstart, stable user-facing features, and evidence-backed feature map.
- `Makefile`: canonical local build, test, lint, generation, and cleanup targets.

Keep reusable logic in `pkg/`; keep command wiring and terminal UI concerns in `cmd/atteler/`.

## Build, Test, and Development Commands

- `make build`: compile the CLI binary as `./atteler`.
- `make run`: run the CLI with `go run ./cmd/atteler`.
- `make test`: run all Go tests with the race detector and no test cache.
- `make lint`: run `golangci-lint` v2.11.4 through `go run`.
- `make generate`: run `go generate ./...`.
- `make all`: run generation, linting, tests, and build in sequence.
- `make clean`: remove the local `atteler` binary.

Use `make test TESTPACKAGE=./pkg/llm` for a faster package-level feedback loop while editing LLM code.
Use `make test TESTFLAGS='-run TestName -count=1' TESTPACKAGE=./pkg/llm` to forward specific `go test` flags.

## Coding Style & Naming Conventions

Use standard Go formatting. Use "make lint" to check for style issues.

Prefer small, focused packages and explicit error wrapping with context. Public exported identifiers need useful comments. Test helpers should call `t.Helper()` when appropriate. Keep provider names lowercase strings such as `"anthropic"` or `"openai"` and test names in the `TestType_Behavior` style already used in `pkg/llm`.

Remember the KISS principles: keep it simple, and avoid over-engineering. Favor readability and maintainability over cleverness or premature abstraction. When in doubt, add a comment explaining the rationale for non-obvious code.

Prefer a factory pattern, and NEVER use singletongs or global state for provider instances. The registry should create new instances on demand, and provider implementations should be stateless and thread-safe.

proper context propagation. Only one context.Background() in the main.go file, everything else should be context-aware and receive a context from its caller.


## Testing Guidelines

Tests use Go's standard `testing` package. Add tests next to the package under test using `_test.go` files. Cover routing, error paths, cancellation, and provider fallback behavior when touching `pkg/llm`. Run `make test` before submitting changes; run `make lint` when changing production code.
Use asserts and require from the "github.com/stretchr/testify/assert" library

## Commit & Pull Request Guidelines

Recent history uses concise conventional prefixes, for example `feat: initial project scaffold`, `docs: add README and Makefile`, and `chore: update notes`. Continue using `feat:`, `fix:`, `docs:`, `test:`, `refactor:`, or `chore:` with a short imperative summary.

Pull requests should include a brief description, the commands run for verification, and any relevant screenshots or terminal output for TUI changes. Link related issues when available and call out follow-up work or known gaps.

## Security & Configuration Tips

Do not commit API keys, local credentials, or generated secrets. Provider authentication should stay behind the existing auth/keychain abstractions in `pkg/llm`. Prefer environment variables or OS keychain integration for local development credentials.
