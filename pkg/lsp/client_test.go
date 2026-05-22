//nolint:wsl_v5 // LSP fake-server tests use compact protocol switch branches.
package lsp

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMain(m *testing.M) {
	if mode := os.Getenv("ATTELER_LSP_FAKE_SERVER"); mode != "" {
		if err := runFakeServer(os.Stdin, os.Stdout, mode); err != nil {
			_, _ = fmt.Fprintln(os.Stderr, err)

			os.Exit(2)
		}

		return
	}

	os.Exit(m.Run())
}

func TestSymbols_RequireActiveContext(t *testing.T) {
	t.Parallel()

	_, err := DocumentSymbols(nil, Options{Command: os.Args[0], FilePath: "missing.go"}) //nolint:staticcheck // Verifies the required-context contract.
	require.Error(t, err)
	assert.Contains(t, err.Error(), "context is required")

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err = WorkspaceSymbols(ctx, Options{Command: os.Args[0], RootPath: t.TempDir()}, "answer")
	require.Error(t, err)
	require.ErrorIs(t, err, context.Canceled)
}

func TestDocumentSymbols_NormalizesDocumentSymbols(t *testing.T) {
	t.Parallel()
	pool := newTestPool(t)
	file := writeTempSource(t, "package main\nfunc main() {}\n")

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()

	symbols, err := DocumentSymbols(ctx, Options{
		Command:  os.Args[0],
		Env:      []string{"ATTELER_LSP_FAKE_SERVER=document"},
		FilePath: file,
		Pool:     pool,
	})

	require.NoError(t, err)
	require.Len(t, symbols, 1)
	assert.Equal(t, "main", symbols[0].Name)
	assert.Equal(t, 12, symbols[0].Kind)
	assert.Equal(t, "func()", symbols[0].Detail)
	assert.Empty(t, symbols[0].URI)
	assert.Equal(t, Range{Start: Position{Line: 1, Character: 0}, End: Position{Line: 1, Character: 14}}, symbols[0].Range)
	require.Len(t, symbols[0].Children, 1)
	assert.Equal(t, "nested", symbols[0].Children[0].Name)
}

func TestDocumentSymbols_NormalizesSymbolInformation(t *testing.T) {
	t.Parallel()
	pool := newTestPool(t)
	file := writeTempSource(t, "package main\nconst answer = 42\n")

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()

	symbols, err := DocumentSymbols(ctx, Options{
		Command:  os.Args[0],
		Env:      []string{"ATTELER_LSP_FAKE_SERVER=info"},
		FilePath: file,
		Pool:     pool,
	})

	require.NoError(t, err)
	require.Len(t, symbols, 1)
	assert.Equal(t, "answer", symbols[0].Name)
	assert.Equal(t, 14, symbols[0].Kind)
	assert.Equal(t, "main", symbols[0].ContainerName)
	assert.NotEmpty(t, symbols[0].URI)
	assert.Equal(t, symbols[0].Range, symbols[0].SelectionRange)
}

func TestWorkspaceSymbols_NormalizesSymbolInformation(t *testing.T) {
	t.Parallel()
	pool := newTestPool(t)

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()

	symbols, err := WorkspaceSymbols(ctx, Options{
		Command:  os.Args[0],
		Env:      []string{"ATTELER_LSP_FAKE_SERVER=workspace"},
		RootPath: t.TempDir(),
		Pool:     pool,
	}, "answer")

	require.NoError(t, err)
	require.Len(t, symbols, 1)
	assert.Equal(t, "answer", symbols[0].Name)
	assert.Equal(t, 14, symbols[0].Kind)
	assert.Equal(t, "main", symbols[0].ContainerName)
	assert.Equal(t, "file:///tmp/main.go", symbols[0].URI)
	assert.Equal(t, symbols[0].Range, symbols[0].SelectionRange)
}

func TestWorkspaceSymbols_WrapsServerErrors(t *testing.T) {
	t.Parallel()
	pool := newTestPool(t)

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()

	_, err := WorkspaceSymbols(ctx, Options{
		Command:  os.Args[0],
		Env:      []string{"ATTELER_LSP_FAKE_SERVER=workspace-error"},
		RootPath: t.TempDir(),
		Pool:     pool,
	}, "answer")

	require.Error(t, err)
	require.ErrorContains(t, err, "request workspace symbols")
	require.ErrorContains(t, err, "language server error")
}

func TestDocumentSymbols_WrapsServerErrors(t *testing.T) {
	t.Parallel()
	pool := newTestPool(t)
	file := writeTempSource(t, "package main\n")

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()

	_, err := DocumentSymbols(ctx, Options{
		Command:  os.Args[0],
		Env:      []string{"ATTELER_LSP_FAKE_SERVER=error"},
		FilePath: file,
		Pool:     pool,
	})

	require.Error(t, err)
	require.ErrorContains(t, err, "request document symbols")
	require.ErrorContains(t, err, "language server error")
}

func TestServerPool_ReusesHealthyServer(t *testing.T) {
	t.Parallel()
	pool := newTestPool(t)
	file := writeTempSource(t, "package main\nfunc main() {}\n")
	launchFile := t.TempDir() + "/launches.log"

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()

	opts := Options{
		Command:  os.Args[0],
		Env:      []string{"ATTELER_LSP_FAKE_SERVER=document", "ATTELER_LSP_FAKE_LAUNCH_FILE=" + launchFile},
		FilePath: file,
		Pool:     pool,
	}

	_, err := pool.DocumentSymbols(ctx, opts)
	require.NoError(t, err)
	_, err = pool.DocumentSymbols(ctx, opts)
	require.NoError(t, err)

	assert.Equal(t, 1, fakeLaunchCount(t, launchFile))
}

func TestServerPool_ReusesServerAfterRequestContextCanceled(t *testing.T) {
	t.Parallel()
	pool := newTestPool(t)
	file := writeTempSource(t, "package main\nfunc main() {}\n")
	launchFile := t.TempDir() + "/launches.log"
	opts := Options{
		Command:  os.Args[0],
		Env:      []string{"ATTELER_LSP_FAKE_SERVER=document", "ATTELER_LSP_FAKE_LAUNCH_FILE=" + launchFile},
		FilePath: file,
		Pool:     pool,
	}

	firstCtx, firstCancel := context.WithTimeout(t.Context(), 5*time.Second)
	_, err := pool.DocumentSymbols(firstCtx, opts)
	require.NoError(t, err)
	firstCancel()

	require.EventuallyWithT(t, func(collect *assert.CollectT) {
		healthy, healthErr := pool.Healthy(opts)
		require.NoError(collect, healthErr)
		assert.True(collect, healthy)
	}, time.Second, 10*time.Millisecond)

	secondCtx, secondCancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer secondCancel()
	_, err = pool.DocumentSymbols(secondCtx, opts)
	require.NoError(t, err)

	assert.Equal(t, 1, fakeLaunchCount(t, launchFile))
}

func TestServerPool_RestartsAfterCrash(t *testing.T) {
	t.Parallel()
	pool := newTestPool(t)
	file := writeTempSource(t, "package main\nfunc main() {}\n")
	launchFile := t.TempDir() + "/launches.log"

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()

	opts := Options{
		Command:  os.Args[0],
		Env:      []string{"ATTELER_LSP_FAKE_SERVER=crash-after-document", "ATTELER_LSP_FAKE_LAUNCH_FILE=" + launchFile},
		FilePath: file,
		Pool:     pool,
	}

	_, err := pool.DocumentSymbols(ctx, opts)
	require.NoError(t, err)
	require.Eventually(t, func() bool {
		logs := pool.ServerLogs()
		return len(logs) == 1 && strings.Contains(logs[0].Stdout, "read json-rpc header")
	}, 5*time.Second, 10*time.Millisecond)

	_, err = pool.DocumentSymbols(ctx, opts)
	require.NoError(t, err)

	assert.Equal(t, 2, fakeLaunchCount(t, launchFile))
}

func TestServerPool_StartAndHealthLifecycle(t *testing.T) {
	t.Parallel()
	pool := newTestPool(t)
	launchFile := t.TempDir() + "/launches.log"
	opts := Options{
		Command:    os.Args[0],
		Env:        []string{"ATTELER_LSP_FAKE_SERVER=workspace", "ATTELER_LSP_FAKE_LAUNCH_FILE=" + launchFile},
		RootPath:   t.TempDir(),
		LanguageID: "go",
		Pool:       pool,
	}

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()

	healthy, err := pool.Healthy(opts)
	require.NoError(t, err)
	assert.False(t, healthy)

	require.NoError(t, pool.Start(ctx, opts))
	healthy, err = pool.Healthy(opts)
	require.NoError(t, err)
	assert.True(t, healthy)

	require.NoError(t, pool.Start(ctx, opts))
	assert.Equal(t, 1, fakeLaunchCount(t, launchFile))

	shutdownCtx, shutdownCancel := context.WithTimeout(context.WithoutCancel(t.Context()), 5*time.Second)
	defer shutdownCancel()
	require.NoError(t, pool.Shutdown(shutdownCtx))

	healthy, err = pool.Healthy(opts)
	require.NoError(t, err)
	assert.False(t, healthy)
}

func TestServerPool_TimesOutAndDropsHungServer(t *testing.T) {
	t.Parallel()
	pool := NewServerPool(PoolOptions{RequestTimeout: 50 * time.Millisecond, ShutdownTimeout: time.Second})
	t.Cleanup(func() {
		shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), 5*time.Second)
		defer cancel()
		require.NoError(t, pool.Shutdown(shutdownCtx))
	})
	file := writeTempSource(t, "package main\nfunc main() {}\n")

	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()

	_, err := pool.DocumentSymbols(ctx, Options{
		Command:  os.Args[0],
		Env:      []string{"ATTELER_LSP_FAKE_SERVER=hang-document"},
		FilePath: file,
		Pool:     pool,
	})

	require.Error(t, err)
	require.ErrorIs(t, err, context.DeadlineExceeded)
	assert.Empty(t, pool.ServerLogs())
}

func TestServerPool_StartTimeoutKillsUninitializedServer(t *testing.T) {
	t.Parallel()
	pool := NewServerPool(PoolOptions{StartTimeout: time.Second, ShutdownTimeout: time.Second})
	file := writeTempSource(t, "package main\nfunc main() {}\n")
	launchFile := t.TempDir() + "/launches.log"

	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()

	_, err := pool.DocumentSymbols(ctx, Options{
		Command:  os.Args[0],
		Env:      []string{"ATTELER_LSP_FAKE_SERVER=hang-initialize", "ATTELER_LSP_FAKE_LAUNCH_FILE=" + launchFile},
		FilePath: file,
		Pool:     pool,
	})

	require.Error(t, err)
	require.ErrorIs(t, err, context.DeadlineExceeded)
	assert.Equal(t, 1, fakeLaunchCount(t, launchFile))
	assert.Empty(t, pool.ServerLogs())
}

func TestServerPool_CapturesAndRedactsServerStderr(t *testing.T) {
	t.Parallel()
	pool := newTestPool(t)
	file := writeTempSource(t, "package main\nfunc main() {}\n")

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()

	_, err := pool.DocumentSymbols(ctx, Options{
		Command: os.Args[0],
		Env: []string{
			"ATTELER_LSP_FAKE_SERVER=document",
			"ATTELER_LSP_FAKE_STDERR=server diagnostic token=topsecret",
			"ATTELER_LSP_SECRET=topsecret",
		},
		FilePath: file,
		Pool:     pool,
	})
	require.NoError(t, err)

	require.Eventually(t, func() bool {
		logs := pool.ServerLogs()
		return len(logs) == 1 &&
			strings.Contains(logs[0].Stderr, "server diagnostic") &&
			strings.Contains(logs[0].Stderr, "token=[REDACTED]") &&
			!strings.Contains(logs[0].Stderr, "topsecret")
	}, 5*time.Second, 10*time.Millisecond)
}

func TestServerPool_CapturesAndRedactsServerStdoutNoise(t *testing.T) {
	t.Parallel()
	pool := newTestPool(t)
	file := writeTempSource(t, "package main\nfunc main() {}\n")

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()

	_, err := pool.DocumentSymbols(ctx, Options{
		Command: os.Args[0],
		Env: []string{
			"ATTELER_LSP_FAKE_SERVER=stdout-noise-after-document",
			"ATTELER_LSP_SECRET=topsecret",
		},
		FilePath: file,
		Pool:     pool,
	})
	require.NoError(t, err)

	require.Eventually(t, func() bool {
		logs := pool.ServerLogs()
		return len(logs) == 1 &&
			strings.Contains(logs[0].Stdout, "token=[REDACTED]") &&
			!strings.Contains(logs[0].Stdout, "topsecret")
	}, 5*time.Second, 10*time.Millisecond)
}

func TestServerPool_CapturesPublishDiagnostics(t *testing.T) {
	t.Parallel()
	pool := newTestPool(t)
	file := writeTempSource(t, "package main\nfunc main() {}\n")

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()

	opts := Options{
		Command:  os.Args[0],
		Env:      []string{"ATTELER_LSP_FAKE_SERVER=diagnostics"},
		FilePath: file,
		Pool:     pool,
	}

	_, err := pool.DocumentSymbols(ctx, opts)
	require.NoError(t, err)
	require.NoError(t, os.Remove(file))

	require.Eventually(t, func() bool {
		diagnostics, err := pool.Diagnostics(opts)
		return err == nil && len(diagnostics) == 1 && diagnostics[0].Message == "fake diagnostic"
	}, 5*time.Second, 10*time.Millisecond)
}

func TestServerPool_UnsupportedDocumentSymbolsTypedError(t *testing.T) {
	t.Parallel()
	pool := newTestPool(t)
	file := writeTempSource(t, "package main\nfunc main() {}\n")

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()

	_, err := pool.DocumentSymbols(ctx, Options{
		Command:  os.Args[0],
		Env:      []string{"ATTELER_LSP_FAKE_SERVER=no-document"},
		FilePath: file,
		Pool:     pool,
	})

	require.Error(t, err)
	var unsupported *UnsupportedCapabilityError
	require.ErrorAs(t, err, &unsupported)
	assert.Equal(t, documentSymbolCapability, unsupported.Method)
}

func TestServerPool_UnsupportedWorkspaceSymbolsTypedError(t *testing.T) {
	t.Parallel()
	pool := newTestPool(t)

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()

	_, err := pool.WorkspaceSymbols(ctx, Options{
		Command:  os.Args[0],
		Env:      []string{"ATTELER_LSP_FAKE_SERVER=no-workspace"},
		RootPath: t.TempDir(),
		Pool:     pool,
	}, "answer")

	require.Error(t, err)
	var unsupported *UnsupportedCapabilityError
	require.ErrorAs(t, err, &unsupported)
	assert.Equal(t, workspaceSymbolCapability, unsupported.Method)
}

func TestServerPool_GracefulShutdownClosesDocumentsAndExits(t *testing.T) {
	t.Parallel()
	pool := NewServerPool(PoolOptions{ShutdownTimeout: 5 * time.Second})
	file := writeTempSource(t, "package main\nfunc main() {}\n")
	eventsFile := t.TempDir() + "/events.log"

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()

	_, err := pool.DocumentSymbols(ctx, Options{
		Command:  os.Args[0],
		Env:      []string{"ATTELER_LSP_FAKE_SERVER=document", "ATTELER_LSP_FAKE_EVENTS_FILE=" + eventsFile},
		FilePath: file,
		Pool:     pool,
	})
	require.NoError(t, err)

	shutdownCtx, shutdownCancel := context.WithTimeout(context.WithoutCancel(t.Context()), 5*time.Second)
	defer shutdownCancel()
	require.NoError(t, pool.Shutdown(shutdownCtx))

	events := readFakeEvents(t, eventsFile)
	assert.Contains(t, events, "didClose")
	assert.Contains(t, events, "shutdown")
	assert.Contains(t, events, "exit")
}

func TestServerPool_DidChangeInvalidatesDocumentSymbolCache(t *testing.T) {
	t.Parallel()
	file := writeTempSource(t, "package main\nfunc main() {}\n")
	launchFile := t.TempDir() + "/launches.log"
	eventsFile := t.TempDir() + "/events.log"
	pool := newTestPool(t)

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()

	opts := Options{
		Command: os.Args[0],
		Env: []string{
			"ATTELER_LSP_FAKE_SERVER=document-from-text",
			"ATTELER_LSP_FAKE_LAUNCH_FILE=" + launchFile,
			"ATTELER_LSP_FAKE_EVENTS_FILE=" + eventsFile,
		},
		FilePath: file,
		Pool:     pool,
	}

	symbols, err := pool.DocumentSymbols(ctx, opts)
	require.NoError(t, err)
	require.Len(t, symbols, 1)
	assert.Equal(t, "main", symbols[0].Name)

	require.NoError(t, os.WriteFile(file, []byte("package main\nfunc changed() {}\n"), 0o600))
	symbols, err = pool.DocumentSymbols(ctx, opts)
	require.NoError(t, err)
	require.Len(t, symbols, 1)
	assert.Equal(t, "changed", symbols[0].Name)

	assert.Equal(t, 1, fakeLaunchCount(t, launchFile))
	assert.Contains(t, readFakeEvents(t, eventsFile), "didChange")
}

func TestServerPool_IncrementalDidChangeUsesWholeDocumentRange(t *testing.T) {
	t.Parallel()
	file := writeTempSource(t, "package main\nfunc main() {}\n")
	eventsFile := t.TempDir() + "/events.log"
	pool := newTestPool(t)

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()

	opts := Options{
		Command: os.Args[0],
		Env: []string{
			"ATTELER_LSP_FAKE_SERVER=document-from-text-incremental",
			"ATTELER_LSP_FAKE_EVENTS_FILE=" + eventsFile,
		},
		FilePath: file,
		Pool:     pool,
	}

	_, err := pool.DocumentSymbols(ctx, opts)
	require.NoError(t, err)

	require.NoError(t, os.WriteFile(file, []byte("package main\nfunc changed() {}\n"), 0o600))
	symbols, err := pool.DocumentSymbols(ctx, opts)
	require.NoError(t, err)
	require.Len(t, symbols, 1)
	assert.Equal(t, "changed", symbols[0].Name)

	events := readFakeEvents(t, eventsFile)
	assert.Contains(t, events, "didChange")
	assert.Contains(t, events, "didChangeRange")
	assert.NotContains(t, events, "didClose")
}

func TestServerPool_ReopensChangedDocumentWhenDidChangeUnsupported(t *testing.T) {
	t.Parallel()
	file := writeTempSource(t, "package main\nfunc main() {}\n")
	launchFile := t.TempDir() + "/launches.log"
	eventsFile := t.TempDir() + "/events.log"
	pool := newTestPool(t)

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()

	opts := Options{
		Command: os.Args[0],
		Env: []string{
			"ATTELER_LSP_FAKE_SERVER=document-from-text-no-change",
			"ATTELER_LSP_FAKE_LAUNCH_FILE=" + launchFile,
			"ATTELER_LSP_FAKE_EVENTS_FILE=" + eventsFile,
		},
		FilePath: file,
		Pool:     pool,
	}

	symbols, err := pool.DocumentSymbols(ctx, opts)
	require.NoError(t, err)
	require.Len(t, symbols, 1)
	assert.Equal(t, "main", symbols[0].Name)

	require.NoError(t, os.WriteFile(file, []byte("package main\nfunc changed() {}\n"), 0o600))
	symbols, err = pool.DocumentSymbols(ctx, opts)
	require.NoError(t, err)
	require.Len(t, symbols, 1)
	assert.Equal(t, "changed", symbols[0].Name)

	events := readFakeEvents(t, eventsFile)
	assert.Equal(t, 1, fakeLaunchCount(t, launchFile))
	assert.Contains(t, events, "didClose")
	assert.NotContains(t, events, "didChange")
}

func TestServerPool_DidOpenInvalidatesWorkspaceSymbolCache(t *testing.T) {
	t.Parallel()
	file := writeTempSource(t, "package main\nfunc main() {}\n")
	pool := newTestPool(t)

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()

	opts := Options{
		Command:    os.Args[0],
		Env:        []string{"ATTELER_LSP_FAKE_SERVER=workspace-from-text"},
		FilePath:   file,
		RootPath:   filepath.Dir(file),
		LanguageID: "go",
		Pool:       pool,
	}

	symbols, err := pool.WorkspaceSymbols(ctx, opts, "answer")
	require.NoError(t, err)
	require.Len(t, symbols, 1)
	assert.Equal(t, "answer", symbols[0].Name)

	_, err = pool.DocumentSymbols(ctx, opts)
	require.NoError(t, err)

	symbols, err = pool.WorkspaceSymbols(ctx, opts, "answer")
	require.NoError(t, err)
	require.Len(t, symbols, 1)
	assert.Equal(t, "main", symbols[0].Name)
}

func TestServerPool_DefinitionsAndReferences(t *testing.T) {
	t.Parallel()
	pool := newTestPool(t)
	file := writeTempSource(t, "package main\nfunc main() {}\n")

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()

	opts := Options{
		Command:  os.Args[0],
		Env:      []string{"ATTELER_LSP_FAKE_SERVER=document"},
		FilePath: file,
		Pool:     pool,
	}

	definitions, err := pool.Definitions(ctx, opts, Position{Line: 1, Character: 5})
	require.NoError(t, err)
	require.Len(t, definitions, 1)
	assert.Equal(t, "file:///tmp/main.go", definitions[0].URI)

	references, err := pool.References(ctx, opts, Position{Line: 1, Character: 5}, true)
	require.NoError(t, err)
	require.Len(t, references, 1)
	assert.Equal(t, definitions[0], references[0])
}

func TestServerPool_UnsupportedDefinitionAndReferenceTypedErrors(t *testing.T) {
	t.Parallel()
	file := writeTempSource(t, "package main\nfunc main() {}\n")

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()

	definitionPool := newTestPool(t)
	_, err := definitionPool.Definitions(ctx, Options{
		Command:  os.Args[0],
		Env:      []string{"ATTELER_LSP_FAKE_SERVER=no-definitions"},
		FilePath: file,
		Pool:     definitionPool,
	}, Position{Line: 1, Character: 5})
	requireUnsupportedCapability(t, err, definitionCapability)

	referencePool := newTestPool(t)
	_, err = referencePool.References(ctx, Options{
		Command:  os.Args[0],
		Env:      []string{"ATTELER_LSP_FAKE_SERVER=no-references"},
		FilePath: file,
		Pool:     referencePool,
	}, Position{Line: 1, Character: 5}, true)
	requireUnsupportedCapability(t, err, referencesCapability)
}

func TestServerPool_RespondsToServerConfigurationRequests(t *testing.T) {
	t.Parallel()
	pool := newTestPool(t)
	file := writeTempSource(t, "package main\nfunc main() {}\n")

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()

	symbols, err := pool.DocumentSymbols(ctx, Options{
		Command:  os.Args[0],
		Env:      []string{"ATTELER_LSP_FAKE_SERVER=server-request"},
		FilePath: file,
		Pool:     pool,
	})

	require.NoError(t, err)
	require.Len(t, symbols, 1)
	assert.Equal(t, "main", symbols[0].Name)
}

func TestServerPool_CommandPolicyDeniesExecution(t *testing.T) {
	t.Parallel()
	file := writeTempSource(t, "package main\nfunc main() {}\n")
	launchFile := t.TempDir() + "/launches.log"
	errDenied := errors.New("denied by test policy")
	pool := NewServerPool(PoolOptions{CommandPolicy: func(_ context.Context, _ CommandSpec) error {
		return errDenied
	}})

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()

	_, err := pool.DocumentSymbols(ctx, Options{
		Command:  os.Args[0],
		Env:      []string{"ATTELER_LSP_FAKE_SERVER=document", "ATTELER_LSP_FAKE_LAUNCH_FILE=" + launchFile},
		FilePath: file,
		Pool:     pool,
	})

	require.ErrorIs(t, err, errDenied)
	assert.Equal(t, 0, fakeLaunchCount(t, launchFile))
}

func TestServerPool_CommandPolicyRunsBeforeHealthyReuse(t *testing.T) {
	t.Parallel()
	pool := newTestPool(t)
	file := writeTempSource(t, "package main\nfunc main() {}\n")
	launchFile := t.TempDir() + "/launches.log"
	errDenied := errors.New("denied on reuse")
	var calls atomic.Int32

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()

	opts := Options{
		Command:  os.Args[0],
		Env:      []string{"ATTELER_LSP_FAKE_SERVER=document", "ATTELER_LSP_FAKE_LAUNCH_FILE=" + launchFile},
		FilePath: file,
		Pool:     pool,
		CommandPolicy: func(_ context.Context, spec CommandSpec) error {
			assert.Equal(t, os.Args[0], spec.Command)
			if calls.Add(1) > 1 {
				return errDenied
			}

			return nil
		},
	}

	_, err := pool.DocumentSymbols(ctx, opts)
	require.NoError(t, err)
	_, err = pool.DocumentSymbols(ctx, opts)
	require.ErrorIs(t, err, errDenied)
	assert.Equal(t, int32(2), calls.Load())
	assert.Equal(t, 1, fakeLaunchCount(t, launchFile))
}

func newTestPool(t *testing.T) *ServerPool {
	t.Helper()
	pool := NewServerPool(PoolOptions{ShutdownTimeout: 5 * time.Second})
	t.Cleanup(func() {
		shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), 5*time.Second)
		defer cancel()
		require.NoError(t, pool.Shutdown(shutdownCtx))
	})

	return pool
}

func writeTempSource(t *testing.T, content string) string {
	t.Helper()
	path := t.TempDir() + "/main.go"
	require.NoError(t, os.WriteFile(path, []byte(content), 0o600))

	return path
}

func fakeLaunchCount(t *testing.T, path string) int {
	t.Helper()
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return 0
	}
	require.NoError(t, err)

	return strings.Count(string(data), "start\n")
}

func readFakeEvents(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return ""
	}
	require.NoError(t, err)

	return string(data)
}

func requireUnsupportedCapability(t *testing.T, err error, method string) {
	t.Helper()
	require.Error(t, err)
	var unsupported *UnsupportedCapabilityError
	require.ErrorAs(t, err, &unsupported)
	assert.Equal(t, method, unsupported.Method)
}

//nolint:govet // Field order keeps JSON-RPC test request fields grouped by message role.
type fakeRequest struct {
	ID      json.RawMessage `json:"id,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	JSONRPC string          `json:"jsonrpc"`
	Method  string          `json:"method"`
	Error   *rpcError       `json:"error,omitempty"`
}

func runFakeServer(input io.Reader, output io.Writer, mode string) error {
	if err := fakeRecord(os.Getenv("ATTELER_LSP_FAKE_LAUNCH_FILE"), "start"); err != nil {
		return err
	}

	if message := os.Getenv("ATTELER_LSP_FAKE_STDERR"); message != "" {
		_, _ = fmt.Fprintln(os.Stderr, message)
	}

	reader := bufio.NewReader(input)
	eventsFile := os.Getenv("ATTELER_LSP_FAKE_EVENTS_FILE")
	didOpen := false
	lastText := ""
	waitingForConfiguration := false

	for {
		payload, err := readFrame(reader)
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}

			return err
		}

		var req fakeRequest
		if err := json.Unmarshal(payload, &req); err != nil {
			return fmt.Errorf("fake server decode request: %w", err)
		}

		if req.Method == "" && waitingForConfiguration {
			if err := fakeValidateConfigurationResponse(req); err != nil {
				return err
			}

			waitingForConfiguration = false
			continue
		}

		switch req.Method {
		case "initialize":
			if mode == "hang-initialize" {
				for {
					time.Sleep(time.Hour)
				}
			}
			if err := fakeValidateInitialize(req.Params); err != nil {
				return err
			}
			if err := fakeRespond(output, req.ID, map[string]any{"capabilities": fakeCapabilities(mode)}, nil); err != nil {
				return err
			}
		case "initialized":
			if mode == "server-request" {
				if err := fakeRequestConfiguration(output); err != nil {
					return err
				}

				waitingForConfiguration = true
			}
		case "textDocument/didOpen":
			text, err := fakeHandleDidOpen(req.Params)
			if err != nil {
				return err
			}

			didOpen = true
			lastText = text
			if err := fakeRecord(eventsFile, "didOpen"); err != nil {
				return err
			}
			if mode == "diagnostics" {
				if err := fakeNotify(output, "textDocument/publishDiagnostics", publishDiagnosticsParams{
					URI: paramsURIFromDidOpen(req.Params),
					Diagnostics: []Diagnostic{{
						Range:   Range{Start: Position{Line: 1, Character: 0}, End: Position{Line: 1, Character: 1}},
						Message: "fake diagnostic",
					}},
				}); err != nil {
					return err
				}
			}
		case "textDocument/didChange":
			text, hasRange, err := fakeHandleDidChange(req.Params, lastText)
			if err != nil {
				return err
			}

			didOpen = true
			lastText = text
			if err := fakeRecord(eventsFile, "didChange"); err != nil {
				return err
			}
			if hasRange {
				if err := fakeRecord(eventsFile, "didChangeRange"); err != nil {
					return err
				}
			}
		case "textDocument/didClose":
			didOpen = false
			if err := fakeRecord(eventsFile, "didClose"); err != nil {
				return err
			}
		case documentSymbolCapability:
			if mode == "hang-document" {
				for {
					time.Sleep(time.Hour)
				}
			}
			if !didOpen {
				return fakeRespond(output, req.ID, nil, &rpcError{Code: -32000, Message: "document was not opened"})
			}

			if err := fakeDocumentSymbolResponse(output, req.ID, mode, lastText); err != nil {
				return err
			}
			if mode == "crash-after-document" {
				return nil
			}
		case workspaceSymbolCapability:
			if err := fakeWorkspaceSymbolResponse(output, req.ID, req.Params, mode, lastText); err != nil {
				return err
			}
		case definitionCapability, referencesCapability:
			if !didOpen {
				return fakeRespond(output, req.ID, nil, &rpcError{Code: -32000, Message: "document was not opened"})
			}
			if err := fakeRespond(output, req.ID, fakeLocations(), nil); err != nil {
				return err
			}
		case "shutdown":
			if err := fakeRecord(eventsFile, "shutdown"); err != nil {
				return err
			}
			if err := fakeRespond(output, req.ID, nil, nil); err != nil {
				return err
			}
		case "exit":
			if err := fakeRecord(eventsFile, "exit"); err != nil {
				return err
			}
			return nil
		default:
			if len(req.ID) > 0 {
				if err := fakeRespond(output, req.ID, nil, &rpcError{Code: -32601, Message: "method not found"}); err != nil {
					return err
				}
			}
		}
	}
}

func fakeRequestConfiguration(output io.Writer) error {
	payload, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      99,
		"method":  "workspace/configuration",
		"params":  map[string]any{"items": []any{}},
	})
	if err != nil {
		return fmt.Errorf("fake server marshal configuration request: %w", err)
	}

	if err := writeFrame(output, payload); err != nil {
		return fmt.Errorf("fake server write configuration request: %w", err)
	}

	return nil
}

func fakeValidateConfigurationResponse(req fakeRequest) error {
	if string(req.ID) != "99" {
		return fmt.Errorf("fake server got configuration response id %s", req.ID)
	}

	if req.Error != nil {
		return fmt.Errorf("fake server got configuration error response: %w", req.Error)
	}

	if string(req.Result) != "[]" {
		return fmt.Errorf("fake server got configuration result %s", req.Result)
	}

	return nil
}

func fakeValidateInitialize(raw json.RawMessage) error {
	var params initializeParams
	if err := json.Unmarshal(raw, &params); err != nil {
		return fmt.Errorf("fake server decode initialize: %w", err)
	}

	if params.RootURI == "" || len(params.WorkspaceFolders) != 1 || params.WorkspaceFolders[0].URI == "" {
		return fmt.Errorf("fake server got incomplete workspace folders: %+v", params)
	}

	return nil
}

func fakeCapabilities(mode string) map[string]any {
	capabilities := map[string]any{
		"textDocumentSync":        map[string]any{"change": 1},
		"documentSymbolProvider":  true,
		"workspaceSymbolProvider": true,
		"definitionProvider":      true,
		"referencesProvider":      true,
	}

	if mode == "no-document" {
		delete(capabilities, "documentSymbolProvider")
	}
	if mode == "no-workspace" {
		delete(capabilities, "workspaceSymbolProvider")
	}
	if mode == "no-definitions" {
		delete(capabilities, "definitionProvider")
	}
	if mode == "no-references" {
		delete(capabilities, "referencesProvider")
	}
	if mode == "document-from-text-no-change" {
		capabilities["textDocumentSync"] = map[string]any{"change": 0}
	}
	if mode == "document-from-text-incremental" {
		capabilities["textDocumentSync"] = map[string]any{"change": 2}
	}

	return capabilities
}

func fakeHandleDidOpen(raw json.RawMessage) (string, error) {
	var params didOpenParams
	if err := json.Unmarshal(raw, &params); err != nil {
		return "", fmt.Errorf("fake server decode didOpen: %w", err)
	}

	if params.TextDocument.URI == "" || params.TextDocument.Text == "" || params.TextDocument.LanguageID != "go" {
		return "", fmt.Errorf("fake server got incomplete didOpen: %+v", params.TextDocument)
	}

	return params.TextDocument.Text, nil
}

func fakeHandleDidChange(raw json.RawMessage, oldText string) (text string, hasRange bool, err error) {
	var params didChangeParams
	if err := json.Unmarshal(raw, &params); err != nil {
		return "", false, fmt.Errorf("fake server decode didChange: %w", err)
	}

	if params.TextDocument.URI == "" || params.TextDocument.Version < 2 || len(params.ContentChanges) != 1 || params.ContentChanges[0].Text == "" {
		return "", false, fmt.Errorf("fake server got incomplete didChange: %+v", params)
	}

	changeRange := params.ContentChanges[0].Range
	if changeRange != nil && *changeRange != fullDocumentRange(oldText) {
		return "", false, fmt.Errorf("fake server got didChange range %+v, want %+v", *changeRange, fullDocumentRange(oldText))
	}

	return params.ContentChanges[0].Text, changeRange != nil, nil
}

func fakeDocumentSymbolResponse(output io.Writer, id json.RawMessage, mode, text string) error {
	switch mode {
	case "document", "crash-after-document", "hang-document", "diagnostics", "server-request", "workspace-from-text":
		return fakeRespond(output, id, fakeDocumentSymbols(), nil)
	case "stdout-noise-after-document":
		if err := fakeRespond(output, id, fakeDocumentSymbols(), nil); err != nil {
			return err
		}

		_, err := fmt.Fprintln(output, "stdout token=topsecret")
		return err
	case "document-from-text", "document-from-text-no-change", "document-from-text-incremental":
		name := "main"
		if strings.Contains(text, "changed") {
			name = "changed"
		}

		return fakeRespond(output, id, fakeDocumentSymbolsNamed(name), nil)
	case "info":
		return fakeRespond(output, id, fakeSymbolInformation(), nil)
	case "error":
		return fakeRespond(output, id, nil, &rpcError{Code: -32603, Message: "boom"})
	default:
		return fmt.Errorf("unknown fake server mode %q", mode)
	}
}

func fakeWorkspaceSymbolResponse(output io.Writer, id, raw json.RawMessage, mode, text string) error {
	var params workspaceSymbolParams
	if err := json.Unmarshal(raw, &params); err != nil {
		return fmt.Errorf("fake server decode workspace symbol: %w", err)
	}

	if params.Query != "answer" {
		return fmt.Errorf("fake server got workspace query %q", params.Query)
	}

	switch mode {
	case "workspace":
		return fakeRespond(output, id, fakeSymbolInformation(), nil)
	case "workspace-from-text":
		name := "answer"
		if strings.Contains(text, "func main") {
			name = "main"
		}

		return fakeRespond(output, id, fakeSymbolInformationNamed(name), nil)
	case "workspace-error":
		return fakeRespond(output, id, nil, &rpcError{Code: -32603, Message: "boom"})
	default:
		return fmt.Errorf("unknown fake server mode %q", mode)
	}
}

func paramsURIFromDidOpen(raw json.RawMessage) string {
	var params didOpenParams
	if err := json.Unmarshal(raw, &params); err != nil {
		return ""
	}

	return params.TextDocument.URI
}

func fakeNotify(output io.Writer, method string, params any) error {
	payload, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "method": method, "params": params})
	if err != nil {
		return fmt.Errorf("fake server marshal notification: %w", err)
	}

	if err := writeFrame(output, payload); err != nil {
		return fmt.Errorf("fake server write notification: %w", err)
	}

	return nil
}

func fakeRecord(path, event string) error {
	if path == "" {
		return nil
	}

	//nolint:gosec // Test helper writes only to temp paths controlled by the tests.
	file, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("fake server open event log: %w", err)
	}
	defer func() { _ = file.Close() }()

	if _, err := fmt.Fprintln(file, event); err != nil {
		return fmt.Errorf("fake server write event log: %w", err)
	}

	return nil
}

func fakeRespond(output io.Writer, id json.RawMessage, result any, rpcErr *rpcError) error {
	response := map[string]any{"jsonrpc": "2.0", "id": id}
	if rpcErr != nil {
		response["error"] = rpcErr
	} else {
		response["result"] = result
	}

	payload, err := json.Marshal(response)
	if err != nil {
		return fmt.Errorf("fake server marshal response: %w", err)
	}

	if err := writeFrame(output, payload); err != nil {
		return fmt.Errorf("fake server write response: %w", err)
	}

	return nil
}

func fakeDocumentSymbols() []documentSymbol {
	return fakeDocumentSymbolsNamed("main")
}

func fakeDocumentSymbolsNamed(name string) []documentSymbol {
	return []documentSymbol{{
		Name:           name,
		Detail:         "func()",
		Kind:           12,
		Range:          Range{Start: Position{Line: 1, Character: 0}, End: Position{Line: 1, Character: 14}},
		SelectionRange: Range{Start: Position{Line: 1, Character: 5}, End: Position{Line: 1, Character: 9}},
		Children: []documentSymbol{{
			Name:           "nested",
			Kind:           13,
			Range:          Range{Start: Position{Line: 1, Character: 10}, End: Position{Line: 1, Character: 12}},
			SelectionRange: Range{Start: Position{Line: 1, Character: 10}, End: Position{Line: 1, Character: 12}},
		}},
	}}
}

func fakeSymbolInformation() []symbolInformation {
	return fakeSymbolInformationNamed("answer")
}

func fakeSymbolInformationNamed(name string) []symbolInformation {
	return []symbolInformation{{
		Name:          name,
		Kind:          14,
		ContainerName: "main",
		Location: Location{
			URI:   "file:///tmp/main.go",
			Range: Range{Start: Position{Line: 1, Character: 6}, End: Position{Line: 1, Character: 12}},
		},
	}}
}

func fakeLocations() []Location {
	return []Location{{
		URI:   "file:///tmp/main.go",
		Range: Range{Start: Position{Line: 1, Character: 6}, End: Position{Line: 1, Character: 12}},
	}}
}
