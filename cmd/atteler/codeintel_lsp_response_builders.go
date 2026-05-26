package main

import (
	"strings"

	"github.com/tommoulard/atteler/pkg/lsp"
)

func buildLSPCodeIntelResponse(input lspSymbolsCommandInput, symbols []lsp.Symbol) codeIntelResponse {
	return finalizeCodeIntelResponse(codeIntelResponse{
		Schema:     codeIntelSchemaVersion,
		Command:    lspSymbolsResponseCommand(input),
		Query:      lspSymbolsQuery(input),
		LSPSymbols: codeIntelLSPSymbolsFromLSP(symbols),
		TextKind:   codeIntelTextLSPSymbols,
	})
}

func lspSymbolsResponseCommand(_ lspSymbolsCommandInput) string {
	// The schema command identifies the registered dispatch command. Workspace
	// symbol requests are a grouped-domain alias for the same providerless
	// command and are distinguished by query.workspace_symbols.
	return codeIntelLSPSymbolsName
}

func lspSymbolsQuery(input lspSymbolsCommandInput) map[string]string {
	query := codeIntelQueryPairs(
		"command", input.Command,
		"file", input.FilePath,
		"root", input.RootPath,
		"language", input.LanguageID,
		"workspace_symbols", input.WorkspaceSymbols,
	)
	if len(input.Args) > 0 {
		if query == nil {
			query = make(map[string]string)
		}

		query["args"] = strings.Join(input.Args, " ")
	}

	return query
}
