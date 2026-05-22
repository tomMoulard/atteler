package lsp

const jsonNull = "null"

const (
	textDocumentSyncNone        = 0
	textDocumentSyncFull        = 1
	textDocumentSyncIncremental = 2
)

//nolint:govet // JSON field order mirrors LSP initialize params.
type initializeParams struct {
	ProcessID        int               `json:"processId"`
	RootPath         string            `json:"rootPath,omitempty"`
	RootURI          string            `json:"rootUri,omitempty"`
	Capabilities     map[string]any    `json:"capabilities"`
	WorkspaceFolders []workspaceFolder `json:"workspaceFolders,omitempty"`
}

type workspaceFolder struct {
	URI  string `json:"uri"`
	Name string `json:"name"`
}

type initializeResult struct {
	Capabilities serverCapabilities `json:"capabilities"`
}

type serverCapabilities struct {
	TextDocumentSync        any `json:"textDocumentSync,omitempty"`
	DocumentSymbolProvider  any `json:"documentSymbolProvider,omitempty"`
	WorkspaceSymbolProvider any `json:"workspaceSymbolProvider,omitempty"`
	DefinitionProvider      any `json:"definitionProvider,omitempty"`
	ReferencesProvider      any `json:"referencesProvider,omitempty"`
}

func (c serverCapabilities) supportsDocumentSymbols() bool {
	return capabilityEnabled(c.DocumentSymbolProvider)
}

func (c serverCapabilities) supportsWorkspaceSymbols() bool {
	return capabilityEnabled(c.WorkspaceSymbolProvider)
}

func (c serverCapabilities) supportsDefinitions() bool {
	return capabilityEnabled(c.DefinitionProvider)
}

func (c serverCapabilities) supportsReferences() bool {
	return capabilityEnabled(c.ReferencesProvider)
}

func (c serverCapabilities) textDocumentSyncKind() int {
	switch sync := c.TextDocumentSync.(type) {
	case float64:
		return int(sync)
	case int:
		return sync
	case bool:
		if sync {
			return textDocumentSyncFull
		}

		return textDocumentSyncNone
	case map[string]any:
		change, ok := sync["change"]
		if !ok {
			return textDocumentSyncNone
		}

		return textDocumentSyncKindValue(change)
	default:
		return textDocumentSyncNone
	}
}

func textDocumentSyncKindValue(value any) int {
	switch typed := value.(type) {
	case float64:
		return int(typed)
	case int:
		return typed
	case bool:
		if typed {
			return textDocumentSyncFull
		}

		return textDocumentSyncNone
	default:
		if capabilityEnabled(value) {
			return textDocumentSyncFull
		}

		return textDocumentSyncNone
	}
}

func capabilityEnabled(value any) bool {
	switch typed := value.(type) {
	case nil:
		return false
	case bool:
		return typed
	case float64:
		return typed > 0
	case int:
		return typed > 0
	case map[string]any:
		return true
	default:
		return true
	}
}

func defaultClientCapabilities() map[string]any {
	return map[string]any{
		"workspace": map[string]any{
			"workspaceFolders": true,
			"symbol": map[string]any{
				"dynamicRegistration": false,
			},
		},
		"textDocument": map[string]any{
			"synchronization": map[string]any{
				"dynamicRegistration": false,
				"didSave":             false,
				"willSave":            false,
			},
			"documentSymbol": map[string]any{
				"dynamicRegistration":               false,
				"hierarchicalDocumentSymbolSupport": true,
			},
			"definition": map[string]any{
				"dynamicRegistration": false,
			},
			"references": map[string]any{
				"dynamicRegistration": false,
			},
			"publishDiagnostics": map[string]any{
				"relatedInformation": true,
			},
		},
	}
}

//nolint:govet // JSON field order mirrors LSP TextDocumentItem.
type textDocumentItem struct {
	URI        string `json:"uri"`
	LanguageID string `json:"languageId"`
	Version    int    `json:"version"`
	Text       string `json:"text"`
}

type didOpenParams struct {
	TextDocument textDocumentItem `json:"textDocument"`
}

type didCloseParams struct {
	TextDocument textDocumentIdentifier `json:"textDocument"`
}

type versionedTextDocumentIdentifier struct {
	URI     string `json:"uri"`
	Version int    `json:"version"`
}

type didChangeParams struct {
	TextDocument   versionedTextDocumentIdentifier  `json:"textDocument"`
	ContentChanges []textDocumentContentChangeEvent `json:"contentChanges"`
}

type textDocumentContentChangeEvent struct {
	Range *Range `json:"range,omitempty"`
	Text  string `json:"text"`
}

type textDocumentIdentifier struct {
	URI string `json:"uri"`
}

type documentSymbolParams struct {
	TextDocument textDocumentIdentifier `json:"textDocument"`
}

type workspaceSymbolParams struct {
	Query string `json:"query"`
}

type textDocumentPositionParams struct {
	TextDocument textDocumentIdentifier `json:"textDocument"`
	Position     Position               `json:"position"`
}

type referenceParams struct {
	TextDocument textDocumentIdentifier `json:"textDocument"`
	Position     Position               `json:"position"`
	Context      referenceContext       `json:"context"`
}

type referenceContext struct {
	IncludeDeclaration bool `json:"includeDeclaration"`
}

type publishDiagnosticsParams struct {
	URI         string       `json:"uri"`
	Diagnostics []Diagnostic `json:"diagnostics"`
}
