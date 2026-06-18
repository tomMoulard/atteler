package main

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestRegisteredCLIFlagSummariesIncludesRegisteredFlags(t *testing.T) {
	t.Parallel()

	flags := registeredCLIFlagSummaries()
	assert.Greater(t, len(flags), 100)
	assertContainsPrefix(t, flags, "--once: send one prompt and exit")
	assertContainsPrefix(t, flags, "--output: output format")
	assertContainsPrefix(t, flags, "--command-surface-json: dump the inspectable CLI command surface")
}

func TestAutopilotCommandSurfaceSummariesIncludeDomainsAndSlashCommands(t *testing.T) {
	t.Parallel()

	commands := commandSurfaceSummaries(buildCommandSurface(commandRegistry).Domains)
	assert.NotEmpty(t, commands)
	assertContainsPrefix(t, commands, "atteler chat/session once")
	assertContainsPrefix(t, commands, "atteler code-intel summary")
	assertContainsPrefix(t, commands, "atteler config commands-json")

	slashCommands := slashCommandSurfaceSummaries(commandSurfaceSlashCommands())
	assert.NotEmpty(t, slashCommands)
	assertContainsPrefix(t, slashCommands, "/model")
}

func assertContainsPrefix(t *testing.T, values []string, prefix string) {
	t.Helper()

	for _, value := range values {
		if strings.HasPrefix(value, prefix) {
			return
		}
	}

	assert.Failf(t, "missing prefix", "no value with prefix %q in %v", prefix, values)
}
