package main

import (
	"context"
	"fmt"
	"strings"

	"github.com/tommoulard/atteler/pkg/session"
)

// commandTier controls at which phase of the dispatch pipeline a command
// runs. Providerless commands run before loadAppState (no LLM registry
// needed); stateful commands run after a full appState is available.
type commandTier int

const (
	tierAny commandTier = -1

	// tierInline commands are handled directly in run() before the registry
	// is consulted (version, config template, etc.).  They are listed here
	// only for documentation; the registry does not store them.
	tierInline commandTier = iota

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
//
//nolint:govet // fields stay grouped by dispatch behavior instead of byte packing.
type command struct {
	// match returns true when the user's flags indicate this command should
	// run. Dispatch validates all matching commands before execution; order
	// only matters when a contract explicitly declares Overrides metadata.
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

	// contract is the inspectable command metadata used by help, JSON dumps,
	// validation, and tests.  It intentionally mirrors dispatch identity
	// instead of making callers reverse-engineer match closures.
	contract commandContract
}

// commandRegistry is the ordered list of non-inline CLI commands.  Dispatch
// validates all matches before execution so ambiguous flag sets fail unless a
// command contract declares explicit precedence.
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

	attachCommandContracts(registry)

	return registry
}

// dispatchProviderless runs the selected providerless command from the
// validated registry. Returns (true, err) if a command was handled,
// (false, nil) if none matched.
func dispatchProviderless(ctx context.Context, opts cliOptions, store *session.Store) (bool, error) {
	cmd, handled, err := selectRegistryCommand(commandRegistry, tierProviderless, opts)
	if err != nil {
		return true, err
	}

	if !handled {
		return false, nil
	}

	return true, cmd.runProviderless(ctx, opts, store)
}

// dispatchProviderlessConfig runs the selected providerless-config command.
// The caller must supply a partially loaded appState (cwd, agent
// registry, plugin paths).
func dispatchProviderlessConfig(ctx context.Context, opts cliOptions, state appState) (bool, error) {
	cmd, handled, err := selectRegistryCommand(commandRegistry, tierProviderlessConfig, opts)
	if err != nil {
		return true, err
	}

	if !handled {
		return false, nil
	}

	return true, cmd.runProviderlessConfig(ctx, opts, state)
}

// dispatchStateful runs the selected stateful command from the validated
// registry.
func dispatchStateful(ctx context.Context, opts cliOptions, state appState) (bool, error) {
	cmd, handled, err := selectRegistryCommand(commandRegistry, tierStateful, opts)
	if err != nil {
		return true, err
	}

	if !handled {
		return false, nil
	}

	return true, cmd.runStateful(ctx, opts, state)
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

func selectRegistryCommand(registry []command, tier commandTier, opts cliOptions) (*command, bool, error) {
	matches := matchingRegistryCommands(registry, tier, opts)
	if len(matches) == 0 {
		return nil, false, nil
	}

	winner, err := resolveCommandAmbiguity(matches)
	if err != nil {
		return nil, true, err
	}

	return winner, true, nil
}

func validateCLICommandSelection(opts cliOptions) error {
	matches := matchingRegistryCommands(commandRegistry, tierAny, opts)
	inlineCommands := buildInlineCommandRegistry()

	matches = append(matches, matchingRegistryCommands(inlineCommands, tierInline, opts)...)
	if _, err := resolveCommandAmbiguity(matches); err != nil {
		return err
	}

	return validateGroupedCommandSelection(opts)
}

func validateGroupedCommandSelection(opts cliOptions) error {
	if _, err := selectCodeIntelCommand(codeIntelCommandInputFromOptions(opts)); err != nil {
		return err
	}

	if _, err := selectStatefulSessionReadCommand(sessionReadCommandInputFromOptions(opts)); err != nil {
		return err
	}

	if _, err := selectStatefulSessionWriteCommand(sessionWriteCommandInputFromOptions(opts)); err != nil {
		return err
	}

	return nil
}

func matchingRegistryCommands(registry []command, tier commandTier, opts cliOptions) []*command {
	matches := make([]*command, 0, 1)

	for i := range registry {
		cmd := &registry[i]
		if tier != tierAny && cmd.tier != tier {
			continue
		}

		if cmd.match(opts) {
			matches = append(matches, cmd)
		}
	}

	return matches
}

func resolveCommandAmbiguity(matches []*command) (*command, error) {
	switch len(matches) {
	case 0:
		return nil, nil
	case 1:
		return matches[0], nil
	}

	if winner := explicitCommandPrecedenceWinner(matches); winner != nil {
		return winner, nil
	}

	return nil, fmt.Errorf("ambiguous CLI command: flags match multiple commands (%s); choose one command or remove conflicting flags",
		matchedCommandSummary(matches))
}

func explicitCommandPrecedenceWinner(matches []*command) *command {
	var winner *command

	for _, candidate := range matches {
		if candidate.contract.coversMatchedCommands(candidate.name, matches) {
			if winner != nil {
				return nil
			}

			winner = candidate
		}
	}

	return winner
}

func matchedCommandSummary(matches []*command) string {
	parts := make([]string, 0, len(matches))
	for _, cmd := range matches {
		flags := cmd.contract.InputFlags
		if len(flags) == 0 {
			parts = append(parts, cmd.name)
			continue
		}

		parts = append(parts, cmd.name+" via "+strings.Join(flags, "/"))
	}

	return strings.Join(parts, ", ")
}
