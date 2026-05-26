//nolint:cyclop,wsl_v5 // Central renderer keeps text output mechanically derived from the response schema.
package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
)

func runCodeIntelSchemaCommand(root string, input codeIntelCommandInput, commandName string) error {
	return runCodeIntelSchemaCommandWithWriter(os.Stdout, root, input, commandName)
}

func runCodeIntelSchemaCommandWithWriter(w io.Writer, root string, input codeIntelCommandInput, commandName string) error {
	format, err := codeIntelOutputFormat(input)
	if err != nil {
		return err
	}

	response, err := buildCodeIntelResponse(root, input, commandName)
	if err != nil {
		return err
	}

	return writeCodeIntelResponse(w, response, format)
}

func writeCodeIntelResponse(w io.Writer, response codeIntelResponse, format string) error {
	response = finalizeCodeIntelResponse(response)

	switch format {
	case outputFormatJSON:
		encoder := json.NewEncoder(w)
		encoder.SetIndent("", "  ")
		if err := encoder.Encode(response); err != nil {
			return fmt.Errorf("encode code-intel json: %w", err)
		}
		return nil
	case outputFormatText:
		return writeCodeIntelText(w, response)
	default:
		return fmt.Errorf("unsupported code-intel output format %q", format)
	}
}

func writeCodeIntelText(w io.Writer, response codeIntelResponse) error {
	if response.Empty {
		return writeCodeIntelLine(w, response.Message)
	}

	if field, ok, known := codeIntelTextPayloadPresent(response); known && !ok {
		return codeIntelMissingTextPayloadError(response.TextKind, field)
	}

	var err error
	switch response.TextKind {
	case codeIntelTextSummary:
		err = writeCodeIntelLine(w, formatCodeIntelSummary(*response.Summary))
	case codeIntelTextFiles:
		err = writeCodeIntelLines(w, len(response.Files), func(i int) string { return formatCodeIntelFile(response.Files[i]) })
	case codeIntelTextFileDetail:
		err = writeCodeIntelFileDetail(w, response.Files[0])
	case codeIntelTextSymbols:
		err = writeCodeIntelLines(w, len(response.Symbols), func(i int) string { return formatCodeIntelSymbol(response.Symbols[i], true) })
	case codeIntelTextFileSymbols:
		err = writeCodeIntelLines(w, len(response.Symbols), func(i int) string { return formatCodeIntelSymbol(response.Symbols[i], false) })
	case codeIntelTextSymbolSummary:
		err = writeCodeIntelLines(w, len(response.Symbols), func(i int) string { return formatCodeIntelSymbolSummary(response.Symbols[i]) })
	case codeIntelTextSymbolFileSummary:
		err = writeCodeIntelLines(w, len(response.Files), func(i int) string { return formatCodeIntelSymbolFileSummary(response.Files[i]) })
	case codeIntelTextPackages:
		err = writeCodeIntelLines(w, len(response.Packages), func(i int) string { return formatCodeIntelPackage(response.Packages[i]) })
	case codeIntelTextImports:
		err = writeCodeIntelLines(w, len(response.Imports), func(i int) string { return "import=" + response.Imports[i].Path })
	case codeIntelTextImportSummary:
		err = writeCodeIntelLines(w, len(response.Imports), func(i int) string { return formatCodeIntelImportSummary(response.Imports[i]) })
	case codeIntelTextImportFileSummary:
		err = writeCodeIntelLines(w, len(response.Files), func(i int) string { return formatCodeIntelImportFileSummary(response.Files[i]) })
	case codeIntelTextPackageImportSummary:
		err = writeCodeIntelLines(w, len(response.Packages), func(i int) string { return formatCodeIntelPackageImportSummary(response.Packages[i]) })
	case codeIntelTextPackageImportMatchSummary:
		err = writeCodeIntelLines(w, len(response.Packages), func(i int) string { return formatCodeIntelPackageImportMatchSummary(response.Packages[i]) })
	case codeIntelTextEdges:
		err = writeCodeIntelLines(w, len(response.Edges), func(i int) string { return formatCodeIntelEdge(response.Edges[i]) })
	case codeIntelTextImpactSet:
		err = writeCodeIntelLines(w, len(response.ImpactSet), func(i int) string { return "path=" + response.ImpactSet[i].Path })
	case codeIntelTextGraphNodes:
		err = writeCodeIntelLines(w, len(response.Nodes), func(i int) string { return "node=" + response.Nodes[i].Path })
	case codeIntelTextCycles:
		err = writeCodeIntelLines(w, len(response.Cycles), func(i int) string { return formatCodeIntelCycle(response.Cycles[i]) })
	case codeIntelTextLayers:
		err = writeCodeIntelLines(w, len(response.Layers), func(i int) string { return formatCodeIntelLayer(response.Layers[i]) })
	case codeIntelTextLSPSymbols:
		err = writeCodeIntelLSPSymbols(w, response.LSPSymbols, 0)
	default:
		err = fmt.Errorf("unsupported code-intel text renderer %q", response.TextKind)
	}

	return err
}

func codeIntelTextPayloadPresent(response codeIntelResponse) (field string, present, known bool) {
	field, known = codeIntelPayloadFieldForKind(response.TextKind)
	if !known {
		return "", false, false
	}

	switch field {
	case string(codeIntelTextSummary):
		return field, response.Summary != nil, true
	case string(codeIntelTextFiles):
		return field, len(response.Files) > 0, true
	case "symbols":
		return field, len(response.Symbols) > 0, true
	case "packages":
		return field, len(response.Packages) > 0, true
	case "imports":
		return field, len(response.Imports) > 0, true
	case "edges":
		return field, len(response.Edges) > 0, true
	case "impact_set":
		return field, len(response.ImpactSet) > 0, true
	case "nodes":
		return field, len(response.Nodes) > 0, true
	case "cycles":
		return field, len(response.Cycles) > 0, true
	case "layers":
		return field, len(response.Layers) > 0, true
	case "lsp_symbols":
		return field, len(response.LSPSymbols) > 0, true
	default:
		return field, false, true
	}
}

func codeIntelMissingTextPayloadError(kind codeIntelTextKind, field string) error {
	return fmt.Errorf("code-intel text renderer %q requires %s payload", kind, field)
}

func writeCodeIntelLine(w io.Writer, line string) error {
	if _, err := fmt.Fprintln(w, line); err != nil {
		return fmt.Errorf("write code-intel text: %w", err)
	}

	return nil
}

func writeCodeIntelLines(w io.Writer, count int, line func(int) string) error {
	for i := range count {
		if err := writeCodeIntelLine(w, line(i)); err != nil {
			return err
		}
	}

	return nil
}

func writeCodeIntelLSPSymbols(w io.Writer, symbols []codeIntelLSPSymbol, depth int) error {
	indent := strings.Repeat("  ", depth)

	for i := range symbols {
		symbol := symbols[i]
		parts := []string{
			indent + symbol.Name,
			"kind=" + strconv.Itoa(symbol.Kind),
			"range=" + formatCodeIntelLSPRange(symbol.Range),
		}
		if symbol.Detail != "" {
			parts = append(parts, "detail="+symbol.Detail)
		}

		if symbol.Container != "" {
			parts = append(parts, "container="+symbol.Container)
		}

		if symbol.URI != "" {
			parts = append(parts, "uri="+symbol.URI)
		}

		if err := writeCodeIntelLine(w, strings.Join(parts, "\t")); err != nil {
			return err
		}

		if err := writeCodeIntelLSPSymbols(w, symbol.Children, depth+1); err != nil {
			return err
		}
	}

	return nil
}

func writeCodeIntelFileDetail(w io.Writer, file codeIntelFile) error {
	if err := writeCodeIntelLine(w, formatCodeIntelFileDetail(file)); err != nil {
		return err
	}

	if len(file.Imports) > 0 {
		if err := writeCodeIntelLine(w, "imports:"); err != nil {
			return err
		}

		for _, imp := range file.Imports {
			if err := writeCodeIntelLine(w, "  - "+imp); err != nil {
				return err
			}
		}
	}

	if len(file.Symbols) > 0 {
		if err := writeCodeIntelLine(w, "symbols:"); err != nil {
			return err
		}

		for i := range file.Symbols {
			if err := writeCodeIntelLine(w, "  - "+formatCodeIntelSymbol(file.Symbols[i], false)); err != nil {
				return err
			}
		}
	}

	return nil
}

func formatCodeIntelSummary(summary codeIntelSummary) string {
	return strings.Join([]string{
		"files=" + strconv.Itoa(summary.Files),
		"packages=" + strconv.Itoa(summary.Packages),
		"symbols=" + strconv.Itoa(summary.Symbols),
		"imports=" + strconv.Itoa(summary.Imports),
		"nodes=" + strconv.Itoa(summary.Nodes),
		"edges=" + strconv.Itoa(summary.Edges),
		"cycles=" + strconv.Itoa(summary.Cycles),
		"layers=" + strconv.Itoa(summary.Layers),
	}, "\t")
}

func formatCodeIntelFile(file codeIntelFile) string {
	return "path=" + file.Path + "\tpackage=" + file.Package + "\tsymbols=" + strconv.Itoa(codeIntelCountValue(file.SymbolCount)) + "\timports=" + strconv.Itoa(codeIntelCountValue(file.ImportCount))
}

func formatCodeIntelFileDetail(file codeIntelFile) string {
	return "path=" + file.Path + "\tpackage=" + file.Package + "\timports=" + strconv.Itoa(codeIntelCountValue(file.ImportCount)) + "\tsymbols=" + strconv.Itoa(codeIntelCountValue(file.SymbolCount))
}

func formatCodeIntelSymbol(symbol codeIntelSymbol, includePath bool) string {
	parts := []string{symbol.Name, "kind=" + symbol.Kind}
	if includePath {
		parts = append(parts, "path="+symbol.Path)
	}
	parts = append(parts, "line="+strconv.Itoa(symbol.Line))

	return strings.Join(parts, "\t")
}

func formatCodeIntelSymbolSummary(symbol codeIntelSymbol) string {
	return "kind=" + symbol.Kind + "\tsymbols=" + strconv.Itoa(symbol.Count)
}

func formatCodeIntelSymbolFileSummary(file codeIntelFile) string {
	return "path=" + file.Path + "\tpackage=" + file.Package + "\tsymbols=" + strconv.Itoa(codeIntelCountValue(file.SymbolCount))
}

func formatCodeIntelPackage(pkg codeIntelPackage) string {
	return "package=" + pkg.Name + "\tfiles=" + strconv.Itoa(codeIntelCountValue(pkg.Files)) + "\tsymbols=" + strconv.Itoa(codeIntelCountValue(pkg.Symbols))
}

func formatCodeIntelImportSummary(imp codeIntelImport) string {
	return "import=" + imp.Path + "\tfiles=" + strconv.Itoa(imp.Files)
}

func formatCodeIntelImportFileSummary(file codeIntelFile) string {
	return "path=" + file.Path + "\tpackage=" + file.Package + "\timports=" + strconv.Itoa(codeIntelCountValue(file.ImportCount))
}

func formatCodeIntelPackageImportSummary(pkg codeIntelPackage) string {
	return "package=" + pkg.Name + "\tfiles=" + strconv.Itoa(codeIntelCountValue(pkg.Files)) + "\timports=" + strconv.Itoa(codeIntelCountValue(pkg.Imports)) + "\tunique_imports=" + strconv.Itoa(codeIntelCountValue(pkg.UniqueImports))
}

func formatCodeIntelPackageImportMatchSummary(pkg codeIntelPackage) string {
	parts := []string{"package=" + pkg.Name, "files=" + strconv.Itoa(codeIntelCountValue(pkg.Files))}
	if pkg.Imports != nil {
		parts = append(parts, "imports="+strconv.Itoa(codeIntelCountValue(pkg.Imports)))
	}

	return strings.Join(parts, "\t")
}

func formatCodeIntelEdge(edge codeIntelEdge) string {
	return "path=" + edge.Path + "\timport=" + edge.Import
}

func formatCodeIntelCycle(cycle codeIntelCycle) string {
	return "cycle=" + strconv.Itoa(cycle.Index) + "\tnodes=" + strings.Join(cycle.Nodes, " -> ")
}

func formatCodeIntelLayer(layer codeIntelLayer) string {
	return "layer=" + strconv.Itoa(layer.Index) + "\tnodes=" + strings.Join(layer.Nodes, ",")
}

func formatCodeIntelLSPRange(r codeIntelLSPRange) string {
	return fmt.Sprintf("%d:%d-%d:%d", r.Start.Line, r.Start.Character, r.End.Line, r.End.Character)
}
