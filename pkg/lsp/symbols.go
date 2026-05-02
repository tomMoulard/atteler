package lsp

import (
	"encoding/json"
	"fmt"
)

//nolint:govet // Field order mirrors LSP DocumentSymbol.
type documentSymbol struct {
	Name           string           `json:"name"`
	Detail         string           `json:"detail"`
	Kind           int              `json:"kind"`
	Range          Range            `json:"range"`
	SelectionRange Range            `json:"selectionRange"`
	Children       []documentSymbol `json:"children"`
}

//nolint:govet // Field order mirrors LSP SymbolInformation.
type symbolInformation struct {
	Name          string   `json:"name"`
	Kind          int      `json:"kind"`
	Location      location `json:"location"`
	ContainerName string   `json:"containerName"`
}

type location struct {
	URI   string `json:"uri"`
	Range Range  `json:"range"`
}

func normalizeSymbols(raw json.RawMessage) ([]Symbol, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	var items []json.RawMessage
	if err := json.Unmarshal(raw, &items); err != nil {
		return nil, fmt.Errorf("decode symbol list: %w", err)
	}
	if len(items) == 0 {
		return nil, nil
	}
	if hasField(items[0], "location") {
		var infos []symbolInformation
		if err := json.Unmarshal(raw, &infos); err != nil {
			return nil, fmt.Errorf("decode SymbolInformation list: %w", err)
		}
		symbols := make([]Symbol, 0, len(infos))
		for _, info := range infos {
			symbols = append(symbols, Symbol{
				Name:           info.Name,
				Kind:           info.Kind,
				ContainerName:  info.ContainerName,
				URI:            info.Location.URI,
				Range:          info.Location.Range,
				SelectionRange: info.Location.Range,
			})
		}
		return symbols, nil
	}

	var docs []documentSymbol
	if err := json.Unmarshal(raw, &docs); err != nil {
		return nil, fmt.Errorf("decode DocumentSymbol list: %w", err)
	}
	return normalizeDocumentSymbols(docs), nil
}

func hasField(raw json.RawMessage, field string) bool {
	var object map[string]json.RawMessage
	if err := json.Unmarshal(raw, &object); err != nil {
		return false
	}
	_, ok := object[field]
	return ok
}

func normalizeDocumentSymbols(docs []documentSymbol) []Symbol {
	symbols := make([]Symbol, 0, len(docs))
	for i := range docs {
		doc := docs[i]
		symbol := Symbol{
			Name:           doc.Name,
			Kind:           doc.Kind,
			Detail:         doc.Detail,
			Range:          doc.Range,
			SelectionRange: doc.SelectionRange,
			Children:       normalizeDocumentSymbols(doc.Children),
		}
		symbols = append(symbols, symbol)
	}
	return symbols
}
