// Package lsp provides small dependency-free Language Server Protocol helpers.
package lsp

import "encoding/json"

// Options configures requests against a managed language server.
//
//nolint:govet // Field order groups command settings before document settings.
type Options struct {
	// Command is the language server executable to run.
	Command string
	// Args are passed to Command.
	Args []string
	// Env appends environment variables for Command.
	Env []string
	// FilePath is the local source file to open and inspect.
	FilePath string
	// RootPath is the workspace root. When empty, FilePath's directory is used
	// for document requests and the current working directory is used for
	// workspace requests.
	RootPath string
	// LanguageID is sent in textDocument/didOpen. When empty, it is inferred from FilePath.
	LanguageID string
	// Pool overrides the package default managed server pool.
	Pool *ServerPool
	// CommandPolicy authorizes this request's language-server command before it
	// can start or reuse a server. PoolOptions.CommandPolicy is checked as well.
	CommandPolicy CommandPolicy
}

// Symbol is a stable, compact representation of either LSP DocumentSymbol or SymbolInformation.
//
//nolint:govet // Field order mirrors public output shape rather than padding.
type Symbol struct {
	Name           string   `json:"name"`
	Kind           int      `json:"kind"`
	Detail         string   `json:"detail,omitempty"`
	ContainerName  string   `json:"containerName,omitempty"`
	URI            string   `json:"uri,omitempty"`
	Range          Range    `json:"range"`
	SelectionRange Range    `json:"selectionRange"`
	Children       []Symbol `json:"children,omitempty"`
}

// Location is an LSP file URI and range pair returned by definition and reference requests.
type Location struct {
	URI   string `json:"uri"`
	Range Range  `json:"range"`
}

// Diagnostic is a captured textDocument/publishDiagnostics item. URI is added
// from the enclosing notification so callers can attribute diagnostics without
// parsing the raw LSP payload.
//
//nolint:govet // Field order mirrors LSP diagnostics.
type Diagnostic struct {
	URI      string          `json:"uri,omitempty"`
	Range    Range           `json:"range"`
	Severity int             `json:"severity,omitempty"`
	Code     json.RawMessage `json:"code,omitempty"`
	Source   string          `json:"source,omitempty"`
	Message  string          `json:"message"`
}

// Position is an LSP zero-based text position.
type Position struct {
	Line      int `json:"line"`
	Character int `json:"character"`
}

// Range is an LSP text range.
type Range struct {
	Start Position `json:"start"`
	End   Position `json:"end"`
}
