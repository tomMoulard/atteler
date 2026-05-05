package subagent

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSpawnAll_RunsRequestsConcurrently(t *testing.T) {
	t.Parallel()

	requests := []Request{
		{ID: "a", Agent: "executor", Prompt: "first"},
		{ID: "b", Agent: "reviewer", Prompt: "second"},
		{ID: "c", Agent: "writer", Prompt: "third"},
	}
	started := make(chan string, len(requests))
	release := make(chan struct{})
	done := make(chan error, 1)

	var results []Result

	go func() {
		var err error

		results, err = SpawnAll(context.Background(), requests, func(ctx context.Context, request Request) (string, error) {
			started <- request.ID

			select {
			case <-release:
				return request.ID + "-output", nil
			case <-ctx.Done():
				return "", ctx.Err()
			}
		})
		done <- err
	}()

	gotStarted := make([]string, 0, len(requests))
	for range requests {
		select {
		case id := <-started:
			gotStarted = append(gotStarted, id)
		case <-time.After(500 * time.Millisecond):
			t.Fatalf("SpawnAll did not start every request concurrently; started %v", gotStarted)
		}
	}

	close(release)

	select {
	case err := <-done:
		require.NoError(t, err)
		require.Len(t, results, len(requests))
		assert.Equal(t, []string{"a", "b", "c"}, resultIDs(results))

		for i := range results {
			assert.Equal(t, requests[i], results[i].Request)
			assert.Equal(t, requests[i].ID+"-output", results[i].Output)
			assert.Empty(t, results[i].Error)
			assert.False(t, results[i].StartedAt.IsZero())
			assert.Positive(t, results[i].Duration)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("SpawnAll did not finish after requests were released")
	}
}

func TestSpawnAll_PreservesInputOrderDespiteCompletionOrder(t *testing.T) {
	t.Parallel()

	requests := []Request{
		{ID: "slow", Agent: "executor", Prompt: "slow"},
		{ID: "fast", Agent: "executor", Prompt: "fast"},
		{ID: "middle", Agent: "executor", Prompt: "middle"},
	}
	results, err := SpawnAll(context.Background(), requests, func(_ context.Context, request Request) (string, error) {
		if request.ID == "slow" {
			time.Sleep(25 * time.Millisecond)
		}

		return request.ID + "-done", nil
	})

	require.NoError(t, err)
	require.Len(t, results, len(requests))
	assert.Equal(t, []string{"slow", "fast", "middle"}, resultIDs(results))
	assert.Equal(t, []string{"slow-done", "fast-done", "middle-done"}, resultOutputs(results))
}

func TestSpawnAll_RecordsErrorsAndReturnsWrappedFailure(t *testing.T) {
	t.Parallel()

	requests := []Request{
		{ID: "ok", Agent: "executor", Prompt: "ok"},
		{ID: "fail", Agent: "executor", Prompt: "fail"},
	}
	results, err := SpawnAll(context.Background(), requests, func(_ context.Context, request Request) (string, error) {
		if request.ID == "fail" {
			return "partial", errors.New("boom")
		}

		return "done", nil
	})

	require.Error(t, err)
	assert.Contains(t, err.Error(), `subagent: request "fail" failed: boom`)
	require.Len(t, results, len(requests))
	assert.Empty(t, results[0].Error)
	assert.Equal(t, "done", results[0].Output)
	assert.Equal(t, "boom", results[1].Error)
	assert.Equal(t, "partial", results[1].Output)
}

func TestSpawnAll_ValidatesInputs(t *testing.T) {
	t.Parallel()

	runner := func(context.Context, Request) (string, error) { return "", nil }

	//nolint:govet // Test case readability is more useful than padding optimization.
	type validationCase struct {
		name     string
		ctx      context.Context
		requests []Request
		runner   Runner
		want     string
	}

	tests := []validationCase{
		{name: "nil context", ctx: nil, requests: []Request{{ID: "a", Agent: "executor", Prompt: "prompt"}}, runner: runner, want: "context is required"},
		{name: "nil runner", ctx: context.Background(), requests: []Request{{ID: "a", Agent: "executor", Prompt: "prompt"}}, runner: nil, want: "runner is required"},
		{name: "missing id", ctx: context.Background(), requests: []Request{{Agent: "executor", Prompt: "prompt"}}, runner: runner, want: "ID is required"},
		{name: "missing agent", ctx: context.Background(), requests: []Request{{ID: "a", Prompt: "prompt"}}, runner: runner, want: "agent is required"},
		{name: "missing prompt", ctx: context.Background(), requests: []Request{{ID: "a", Agent: "executor"}}, runner: runner, want: "prompt is required"},
		{name: "duplicate id", ctx: context.Background(), requests: []Request{{ID: "a", Agent: "executor", Prompt: "one"}, {ID: "a", Agent: "writer", Prompt: "two"}}, runner: runner, want: `duplicate request ID "a"`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			results, err := SpawnAll(tt.ctx, tt.requests, tt.runner)
			require.Error(t, err)
			assert.Nil(t, results)
			assert.Contains(t, err.Error(), tt.want)
		})
	}
}

func TestAttelerCommand_ConstructsExpectedArguments(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	argsFile := filepath.Join(dir, "args.txt")
	fake := filepath.Join(dir, "fake-atteler")
	writeFakeCommand(t, fake, `#!/bin/sh
printf '%s\n' "$@" > "$ATTELER_ARGS_FILE"
printf 'ran:%s:%s' "$2" "$4"
`)

	runner := AttelerCommandWithOptions(CommandOptions{
		Binary: fake,
		Env:    map[string]string{"ATTELER_ARGS_FILE": argsFile},
	})

	output, err := runner(context.Background(), Request{ID: "child", Agent: "architect", Prompt: "draft plan"})

	require.NoError(t, err)
	assert.Equal(t, "ran:architect:draft plan", output)

	contents, err := os.ReadFile(argsFile)
	require.NoError(t, err)
	assert.Equal(t, []string{"--agent", "architect", "--once", "draft plan"}, strings.Split(strings.TrimSpace(string(contents)), "\n"))
}

func TestAttelerCommand_PrependsConfiguredArguments(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	argsFile := filepath.Join(dir, "args.txt")
	fake := filepath.Join(dir, "fake-atteler")
	writeFakeCommand(t, fake, `#!/bin/sh
printf '%s\n' "$@" > "$ATTELER_ARGS_FILE"
`)

	runner := AttelerCommandWithOptions(CommandOptions{
		Args:   []string{"--model", "codex/gpt-5.5", "--session-dir", dir},
		Binary: fake,
		Env:    map[string]string{"ATTELER_ARGS_FILE": argsFile},
	})

	_, err := runner(context.Background(), Request{ID: "child", Agent: "architect", Prompt: "draft plan"})

	require.NoError(t, err)

	contents, err := os.ReadFile(argsFile)
	require.NoError(t, err)
	assert.Equal(t, []string{
		"--model",
		"codex/gpt-5.5",
		"--session-dir",
		dir,
		"--agent",
		"architect",
		"--once",
		"draft plan",
	}, strings.Split(strings.TrimSpace(string(contents)), "\n"))
}

func TestAttelerCommand_ReturnsOutputAndWrappedCommandError(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	fake := filepath.Join(dir, "fake-atteler")
	writeFakeCommand(t, fake, `#!/bin/sh
printf partial
printf 'bad request' >&2
exit 7
`)

	output, err := AttelerCommand(fake)(context.Background(), Request{ID: "child", Agent: "architect", Prompt: "draft plan"})

	require.Error(t, err)
	assert.Equal(t, "partial", output)
	assert.Contains(t, err.Error(), "atteler command failed: bad request")
}

func writeFakeCommand(t *testing.T, path, contents string) {
	t.Helper()

	//nolint:gosec // Test fixtures must be executable by the spawned process.
	require.NoError(t, os.WriteFile(path, []byte(contents), 0o755))
}

func resultIDs(results []Result) []string {
	ids := make([]string, len(results))
	for i := range results {
		ids[i] = results[i].Request.ID
	}

	return ids
}

func resultOutputs(results []Result) []string {
	outputs := make([]string, len(results))
	for i := range results {
		outputs[i] = results[i].Output
	}

	return outputs
}
