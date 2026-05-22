package lsp

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
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
	file := writeTempSource(t, "package main\nfunc main() {}\n")

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()

	symbols, err := DocumentSymbols(ctx, Options{
		Command:  os.Args[0],
		Env:      []string{"ATTELER_LSP_FAKE_SERVER=document"},
		FilePath: file,
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
	file := writeTempSource(t, "package main\nconst answer = 42\n")

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()

	symbols, err := DocumentSymbols(ctx, Options{
		Command:  os.Args[0],
		Env:      []string{"ATTELER_LSP_FAKE_SERVER=info"},
		FilePath: file,
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

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()

	symbols, err := WorkspaceSymbols(ctx, Options{
		Command:  os.Args[0],
		Env:      []string{"ATTELER_LSP_FAKE_SERVER=workspace"},
		RootPath: t.TempDir(),
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

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()

	_, err := WorkspaceSymbols(ctx, Options{
		Command:  os.Args[0],
		Env:      []string{"ATTELER_LSP_FAKE_SERVER=workspace-error"},
		RootPath: t.TempDir(),
	}, "answer")

	require.Error(t, err)
	require.ErrorContains(t, err, "request workspace symbols")
	require.ErrorContains(t, err, "language server error")
}

func TestDocumentSymbols_WrapsServerErrors(t *testing.T) {
	t.Parallel()
	file := writeTempSource(t, "package main\n")

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()

	_, err := DocumentSymbols(ctx, Options{
		Command:  os.Args[0],
		Env:      []string{"ATTELER_LSP_FAKE_SERVER=error"},
		FilePath: file,
	})

	require.Error(t, err)
	require.ErrorContains(t, err, "request document symbols")
	require.ErrorContains(t, err, "language server error")
}

func writeTempSource(t *testing.T, content string) string {
	t.Helper()
	path := t.TempDir() + "/main.go"
	require.NoError(t, os.WriteFile(path, []byte(content), 0o600))

	return path
}

type fakeRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

func runFakeServer(input io.Reader, output io.Writer, mode string) error {
	reader := bufio.NewReader(input)
	didOpen := false

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

		switch req.Method {
		case "initialize":
			if err := fakeRespond(output, req.ID, map[string]any{"capabilities": map[string]any{"documentSymbolProvider": true, "workspaceSymbolProvider": true}}, nil); err != nil {
				return err
			}
		case "initialized":
		case "textDocument/didOpen":
			var params didOpenParams
			if err := json.Unmarshal(req.Params, &params); err != nil {
				return fmt.Errorf("fake server decode didOpen: %w", err)
			}

			if params.TextDocument.URI == "" || params.TextDocument.Text == "" || params.TextDocument.LanguageID != "go" {
				return fmt.Errorf("fake server got incomplete didOpen: %+v", params.TextDocument)
			}

			didOpen = true
		case "textDocument/documentSymbol":
			if !didOpen {
				return fakeRespond(output, req.ID, nil, &rpcError{Code: -32000, Message: "document was not opened"})
			}

			switch mode {
			case "document":
				if err := fakeRespond(output, req.ID, fakeDocumentSymbols(), nil); err != nil {
					return err
				}
			case "info":
				if err := fakeRespond(output, req.ID, fakeSymbolInformation(), nil); err != nil {
					return err
				}
			case "error":
				if err := fakeRespond(output, req.ID, nil, &rpcError{Code: -32603, Message: "boom"}); err != nil {
					return err
				}
			default:
				return fmt.Errorf("unknown fake server mode %q", mode)
			}
		case "workspace/symbol":
			var params workspaceSymbolParams
			if err := json.Unmarshal(req.Params, &params); err != nil {
				return fmt.Errorf("fake server decode workspace symbol: %w", err)
			}

			if params.Query != "answer" {
				return fmt.Errorf("fake server got workspace query %q", params.Query)
			}

			switch mode {
			case "workspace":
				if err := fakeRespond(output, req.ID, fakeSymbolInformation(), nil); err != nil {
					return err
				}
			case "workspace-error":
				if err := fakeRespond(output, req.ID, nil, &rpcError{Code: -32603, Message: "boom"}); err != nil {
					return err
				}
			default:
				return fmt.Errorf("unknown fake server mode %q", mode)
			}
		case "shutdown":
			if err := fakeRespond(output, req.ID, nil, nil); err != nil {
				return err
			}
		case "exit":
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
	return []documentSymbol{{
		Name:           "main",
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
	return []symbolInformation{{
		Name:          "answer",
		Kind:          14,
		ContainerName: "main",
		Location: location{
			URI:   "file:///tmp/main.go",
			Range: Range{Start: Position{Line: 1, Character: 6}, End: Position{Line: 1, Character: 12}},
		},
	}}
}
