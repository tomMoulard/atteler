package main

import (
	"context"

	"github.com/tommoulard/atteler/pkg/session"
)

// commandTier controls at which phase of the dispatch pipeline a command
// runs. Providerless commands run before loadAppState (no LLM registry
// needed); stateful commands run after a full appState is available.
type commandTier int

const (
	// tierInline commands are handled directly in run() before the registry
	// is consulted (version, config template, etc.).  They are listed here
	// only for documentation; the registry does not store them.
	_ commandTier = iota

	// tierProviderless commands need only a session.Store and cwd -- no LLM
	// providers.  They run before loadAppState to keep startup fast.
	tierProviderless

	// tierProviderlessConfig commands need a session.Store plus loaded
	// config (agent registry, plugin paths) but still no LLM provider.
	tierProviderlessConfig

	// tierStateful commands require a fully initialized appState including
	// an LLM registry and hook runner.
	tierStateful
)

// command describes a single dispatchable CLI action.
type command struct {
	// match returns true when the user's flags indicate this command should
	// run.  Commands are checked in registration order; the first match wins.
	match func(cliOptions) bool

	// runProviderless is called for tierProviderless commands.
	runProviderless func(ctx context.Context, opts cliOptions, store *session.Store) error

	// runProviderlessConfig is called for tierProviderlessConfig commands.
	runProviderlessConfig func(ctx context.Context, opts cliOptions, state appState) error

	// runStateful is called for tierStateful commands.
	runStateful func(ctx context.Context, opts cliOptions, state appState) error

	// name is a human-readable label for debugging / logging.
	name string

	// tier controls when in the dispatch pipeline this command runs.
	tier commandTier
}

// commandRegistry is the ordered list of all CLI commands.  The first command
// whose match function returns true wins.  The order within each tier matters
// for compound conditions (e.g. MCP invoke before MCP manifest).
//
//nolint:gochecknoglobals // registry is initialized once at program start and never mutated.
var commandRegistry = buildCommandRegistry()

func buildCommandRegistry() []command {
	groups := [][]command{
		providerlessSessionCommands(),
		providerlessFileCommands(),
		providerlessPlanningCommands(),
		providerlessConfigAgentPluginCommands(),
		providerlessConfigCodeIntelCommands(),
		providerlessConfigLocalAnalysisCommands(),
		statefulSessionReadCommands(),
		statefulSessionWriteCommands(),
		statefulExecutionCommands(),
		statefulRetrievalCommands(),
		statefulLocalAnalysisCommands(),
		statefulProviderCommands(),
	}

	total := 0
	for _, group := range groups {
		total += len(group)
	}

	registry := make([]command, 0, total)
	for _, group := range groups {
		registry = append(registry, group...)
	}

	return registry
}

// dispatchProviderless runs the first matching providerless command from the
// registry.  Returns (true, err) if a command was handled, (false, nil) if
// none matched.
func dispatchProviderless(ctx context.Context, opts cliOptions, store *session.Store) (bool, error) {
	for i := range commandRegistry {
		cmd := &commandRegistry[i]
		if cmd.tier != tierProviderless || !cmd.match(opts) {
			continue
		}

		return true, cmd.runProviderless(ctx, opts, store)
	}

	return false, nil
}

// dispatchProviderlessConfig runs the first matching providerless-config
// command.  The caller must supply a partially loaded appState (cwd, agent
// registry, plugin paths).
func dispatchProviderlessConfig(ctx context.Context, opts cliOptions, state appState) (bool, error) {
	for i := range commandRegistry {
		cmd := &commandRegistry[i]
		if cmd.tier != tierProviderlessConfig || !cmd.match(opts) {
			continue
		}

		return true, cmd.runProviderlessConfig(ctx, opts, state)
	}

	return false, nil
}

// dispatchStateful runs the first matching stateful command from the
// registry.
func dispatchStateful(ctx context.Context, opts cliOptions, state appState) (bool, error) {
	for i := range commandRegistry {
		cmd := &commandRegistry[i]
		if cmd.tier != tierStateful || !cmd.match(opts) {
			continue
		}

		return true, cmd.runStateful(ctx, opts, state)
	}

	return false, nil
}

// providerlessConfigRequested returns true if any providerless-config
// command matches the current options.
func providerlessConfigRequested(opts cliOptions) bool {
	for i := range commandRegistry {
		cmd := &commandRegistry[i]
		if cmd.tier == tierProviderlessConfig && cmd.match(opts) {
			return true
		}
	}

	return false
}
