// Package lsp provides small dependency-free Language Server Protocol helpers.
package lsp

// Options configures a one-shot document symbols request against a language server.
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
	// RootPath is the workspace root. When empty, FilePath's directory is used.
	RootPath string
	// LanguageID is sent in textDocument/didOpen. When empty, it is inferred from FilePath.
	LanguageID string
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
