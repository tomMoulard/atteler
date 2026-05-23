package main

import (
	"fmt"
	"strings"

	"github.com/tommoulard/atteler/pkg/codeintel"
)

type codeIntelResponseBuilder func(string, codeintel.Index, codeIntelCommandInput, codeIntelResponse, string) (codeIntelResponse, bool, error)

func buildCodeIntelResponse(root string, input codeIntelCommandInput, commandName string) (codeIntelResponse, error) {
	textKind := codeIntelTextKindForCommand(commandName)
	if textKind == "" {
		return codeIntelResponse{}, fmt.Errorf("unsupported code-intel command %q", commandName)
	}

	idx, err := codeintel.IndexDir(root)
	if err != nil {
		return codeIntelResponse{}, fmt.Errorf("%s: index %s: %w", commandName, root, err)
	}

	response := newCodeIntelResponse(commandName)
	response.TextKind = textKind

	builders := []codeIntelResponseBuilder{
		buildCodeIntelRepositoryResponse,
		buildCodeIntelGraphResponse,
		buildCodeIntelImportResponse,
		buildCodeIntelPackageImportPathResponse,
		buildCodeIntelPackageImportPrefixResponse,
		buildCodeIntelPackageImportSummaryResponse,
		buildCodeIntelSymbolResponse,
		buildCodeIntelPackageSymbolSummaryResponse,
		buildCodeIntelPackageSymbolFilterResponse,
		buildCodeIntelFileImportResponse,
		buildCodeIntelFileSymbolResponse,
	}

	for _, builder := range builders {
		built, handled, err := builder(root, idx, input, response, commandName)
		if err != nil {
			return built, err
		}

		if handled {
			return finalizeCodeIntelResponse(paginateCodeIntelResponse(built, input)), nil
		}
	}

	return response, fmt.Errorf("unsupported code-intel command %q", commandName)
}

func codeIntelQuery(key, value string) map[string]string {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil
	}

	return map[string]string{key: value}
}

func codeIntelQueryPairs(values ...string) map[string]string {
	query := make(map[string]string, len(values)/2)
	for i := 0; i+1 < len(values); i += 2 {
		value := strings.TrimSpace(values[i+1])
		if value != "" {
			query[values[i]] = value
		}
	}

	if len(query) == 0 {
		return nil
	}

	return query
}

func paginateCodeIntelResponse(response codeIntelResponse, input codeIntelCommandInput) codeIntelResponse {
	limit := input.Limit
	offset := max(input.Offset, 0)

	if limit <= 0 && offset <= 0 {
		return response
	}

	if !codeIntelTextKindSupportsPagination(response.TextKind) {
		return response
	}

	switch {
	case len(response.Files) > 0:
		total := len(response.Files)
		response.Files = paginateCodeIntelSlice(response.Files, limit, offset)
		response.Pagination = newCodeIntelPagination(limit, offset, total, len(response.Files))
	case len(response.Packages) > 0:
		total := len(response.Packages)
		response.Packages = paginateCodeIntelSlice(response.Packages, limit, offset)
		response.Pagination = newCodeIntelPagination(limit, offset, total, len(response.Packages))
	case len(response.Symbols) > 0:
		total := len(response.Symbols)
		response.Symbols = paginateCodeIntelSlice(response.Symbols, limit, offset)
		response.Pagination = newCodeIntelPagination(limit, offset, total, len(response.Symbols))
	case len(response.Imports) > 0:
		total := len(response.Imports)
		response.Imports = paginateCodeIntelSlice(response.Imports, limit, offset)
		response.Pagination = newCodeIntelPagination(limit, offset, total, len(response.Imports))
	case len(response.Edges) > 0:
		total := len(response.Edges)
		response.Edges = paginateCodeIntelSlice(response.Edges, limit, offset)
		response.Pagination = newCodeIntelPagination(limit, offset, total, len(response.Edges))
	case len(response.ImpactSet) > 0:
		total := len(response.ImpactSet)
		response.ImpactSet = paginateCodeIntelSlice(response.ImpactSet, limit, offset)
		response.Pagination = newCodeIntelPagination(limit, offset, total, len(response.ImpactSet))
	case len(response.Nodes) > 0:
		total := len(response.Nodes)
		response.Nodes = paginateCodeIntelSlice(response.Nodes, limit, offset)
		response.Pagination = newCodeIntelPagination(limit, offset, total, len(response.Nodes))
	case len(response.Cycles) > 0:
		total := len(response.Cycles)
		response.Cycles = paginateCodeIntelSlice(response.Cycles, limit, offset)
		response.Pagination = newCodeIntelPagination(limit, offset, total, len(response.Cycles))
	case len(response.Layers) > 0:
		total := len(response.Layers)
		response.Layers = paginateCodeIntelSlice(response.Layers, limit, offset)
		response.Pagination = newCodeIntelPagination(limit, offset, total, len(response.Layers))
	default:
		response.Pagination = newCodeIntelPagination(limit, offset, 0, 0)
	}

	return response
}

func newCodeIntelPagination(limit, offset, total, returned int) *codeIntelPagination {
	pagination := &codeIntelPagination{
		Offset:   offset,
		Total:    total,
		Returned: returned,
	}
	if limit > 0 {
		pagination.Limit = new(limit)
	}

	return pagination
}
