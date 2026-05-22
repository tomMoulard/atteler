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
	Location      Location `json:"location"`
	ContainerName string   `json:"containerName"`
}

type locationLink struct {
	TargetURI            string `json:"targetUri"`
	TargetRange          Range  `json:"targetRange"`
	TargetSelectionRange Range  `json:"targetSelectionRange"`
}

func normalizeSymbols(raw json.RawMessage) ([]Symbol, error) {
	if len(raw) == 0 || string(raw) == jsonNull {
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

func normalizeLocations(raw json.RawMessage) ([]Location, error) {
	if len(raw) == 0 || string(raw) == jsonNull {
		return nil, nil
	}

	if raw[0] == '[' {
		var items []json.RawMessage
		if err := json.Unmarshal(raw, &items); err != nil {
			return nil, fmt.Errorf("decode location list: %w", err)
		}

		locations := make([]Location, 0, len(items))
		for _, item := range items {
			loc, err := normalizeLocation(item)
			if err != nil {
				return nil, err
			}

			locations = append(locations, loc)
		}

		return locations, nil
	}

	loc, err := normalizeLocation(raw)
	if err != nil {
		return nil, err
	}

	return []Location{loc}, nil
}

func normalizeLocation(raw json.RawMessage) (Location, error) {
	if hasField(raw, "targetUri") {
		var link locationLink
		if err := json.Unmarshal(raw, &link); err != nil {
			return Location{}, fmt.Errorf("decode LocationLink: %w", err)
		}

		rangeValue := link.TargetSelectionRange
		if rangeValue == (Range{}) {
			rangeValue = link.TargetRange
		}

		return Location{URI: link.TargetURI, Range: rangeValue}, nil
	}

	var loc Location
	if err := json.Unmarshal(raw, &loc); err != nil {
		return Location{}, fmt.Errorf("decode Location: %w", err)
	}

	return loc, nil
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

func cloneSymbols(in []Symbol) []Symbol {
	out := append([]Symbol(nil), in...)
	for i := range out {
		out[i].Children = cloneSymbols(out[i].Children)
	}

	return out
}
