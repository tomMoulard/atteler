# Atteler SDK examples

Each subdirectory is a small Go `main` package that compiles with
`go test ./examples/...` and demonstrates one stable SDK workflow without live
provider credentials.

| Directory | Workflow |
| --- | --- |
| `one-shot-chat` | Register a provider and run `sdk.RunOneShotChat`. |
| `provider-registry` | Build an `llm.Registry` through `sdk.NewProviderRegistry`. |
| `review-run` | Create a deterministic review plan and render its contract. |
| `memory-search` | Index local documents and run lexical memory search. |
| `plugin-execution` | Execute a governed local plugin entrypoint. |
| `worktree-session` | Create and save a session, optionally attaching a git worktree with `ATTELER_EXAMPLE_REPO`. |
