package atteler_test

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMakeTestDefaultKeepsCIRaceNoCache(t *testing.T) {
	t.Parallel()

	got := makeDryRunCommand(t, "test")

	require.Equal(t, "go test -race -count=1 ./...", got)
}

func TestMakeTestForwardsPackageAndFlags(t *testing.T) {
	t.Parallel()

	got := makeDryRunCommand(t, "test", "TESTPACKAGE=./pkg/llm", "TESTFLAGS=-run TestName -count=1")

	require.Equal(t, "go test -race -count=1 -run TestName -count=1 ./pkg/llm", got)
}

func TestMakeTestPreservesLiteralDollarInFlags(t *testing.T) {
	t.Parallel()

	got := makeDryRunCommand(t, "test", "TESTPACKAGE=./pkg/llm", "TESTFLAGS=-run ^TestName$ -count=1")

	require.Equal(t, "go test -race -count=1 -run ^TestName$ -count=1 ./pkg/llm", got)
}

func TestMakeE2ETargetsForwardTestFlags(t *testing.T) {
	t.Parallel()

	got := makeDryRunCommand(t, "e2e", "TESTFLAGS=-run TestCLIHelp")
	require.Equal(t, "go test -count=1 -run TestCLIHelp ./test/e2e", got)

	got = makeDryRunCommand(t, "e2e-live", "TESTFLAGS=-v")
	require.Equal(t, "ATTELER_E2E_LIVE=1 go test -count=1 -run TestLive -timeout=10m -v ./test/e2e", got)
}

func TestContributorDocsMentionFocusedMakeTestKnobs(t *testing.T) {
	t.Parallel()

	for _, path := range []string{"AGENTS.md", "README.md"} {
		t.Run(path, func(t *testing.T) {
			t.Parallel()

			contents, err := os.ReadFile(path)
			require.NoError(t, err)

			text := string(contents)
			assert.Contains(t, text, "make test TESTPACKAGE=./pkg/llm")
			assert.Contains(t, text, "make test TESTFLAGS='-run TestName -count=1' TESTPACKAGE=./pkg/llm")
		})
	}
}

func makeDryRunCommand(t *testing.T, args ...string) string {
	t.Helper()

	if _, err := exec.LookPath("make"); err != nil {
		t.Skipf("make is unavailable: %v", err)
	}

	cmdArgs := append([]string{"--dry-run"}, args...)

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, "make", cmdArgs...)
	cmd.Env = withoutInheritedMakeVars(os.Environ())

	output, err := cmd.CombinedOutput()
	require.NoError(t, err, "make --dry-run %s failed:\n%s", strings.Join(args, " "), output)

	for line := range strings.SplitSeq(string(output), "\n") {
		if strings.Contains(line, "go test") {
			return strings.Join(strings.Fields(line), " ")
		}
	}

	require.Failf(t, "missing go test command", "make --dry-run %s output:\n%s", strings.Join(args, " "), output)

	return ""
}

func withoutInheritedMakeVars(env []string) []string {
	filtered := make([]string, 0, len(env)+2)
	for _, entry := range env {
		if strings.HasPrefix(entry, "MAKEFLAGS=") ||
			strings.HasPrefix(entry, "MFLAGS=") ||
			strings.HasPrefix(entry, "TESTFLAGS=") ||
			strings.HasPrefix(entry, "TESTPACKAGE=") {
			continue
		}

		filtered = append(filtered, entry)
	}

	return append(filtered, "MAKEFLAGS=", "MFLAGS=")
}
