# Hooks

> Lifecycle events you can subscribe to from YAML.

Atteler dispatches lifecycle events to local hooks configured under `hooks:` in
YAML config (`pkg/events`). Use `--list-hook-events` or `--list-hook-events-json`
to introspect the supported types.

## Supported events

<!-- The table below is generated from `atteler --list-hook-events-json`. -->

--8<-- "generated/hook-events.md"

## Payloads and examples

Hook payloads are filtered through a `payload` policy (`metadata`, `summary`, or
`full`) before local hooks receive them. For the full schema and example
payloads for every event under each policy, see
**[Lifecycle events](lifecycle-events.md)**.
