package main

const (
	codeIntelSchemaVersion = "atteler.code_intel.v1"
	codeIntelEmptyMessage  = "No code-intel results found."
)

type codeIntelTextKind string

const (
	codeIntelTextSummary                   codeIntelTextKind = "summary"
	codeIntelTextFiles                     codeIntelTextKind = "files"
	codeIntelTextFileDetail                codeIntelTextKind = "file_detail"
	codeIntelTextSymbols                   codeIntelTextKind = "symbols"
	codeIntelTextFileSymbols               codeIntelTextKind = "file_symbols"
	codeIntelTextSymbolSummary             codeIntelTextKind = "symbol_summary"
	codeIntelTextSymbolFileSummary         codeIntelTextKind = "symbol_file_summary"
	codeIntelTextPackages                  codeIntelTextKind = "packages"
	codeIntelTextImports                   codeIntelTextKind = "imports"
	codeIntelTextImportSummary             codeIntelTextKind = "import_summary"
	codeIntelTextImportFileSummary         codeIntelTextKind = "import_file_summary"
	codeIntelTextPackageImportSummary      codeIntelTextKind = "package_import_summary"
	codeIntelTextPackageImportMatchSummary codeIntelTextKind = "package_import_match_summary"
	codeIntelTextEdges                     codeIntelTextKind = "edges"
	codeIntelTextImpactSet                 codeIntelTextKind = "impact_set"
	codeIntelTextGraphNodes                codeIntelTextKind = "graph_nodes"
	codeIntelTextCycles                    codeIntelTextKind = "cycles"
	codeIntelTextLayers                    codeIntelTextKind = "layers"
	codeIntelTextLSPSymbols                codeIntelTextKind = "lsp_symbols"
	codeIntelTextQuery                     codeIntelTextKind = "query"
)

// codeIntelResponse is the stable structured contract for code-intel command output.
// Text output is rendered from these same typed fields instead of from command-local printers.
//
//nolint:govet // JSON field grouping is optimized for readability over pointer-byte packing.
type codeIntelResponse struct {
	Schema string `json:"schema"`
	// Command is the stable code-intel dispatch/query descriptor name. It may be
	// more specific than the grouped CLI word so automation can distinguish
	// variants such as symbol and symbol file summary; grouped aliases that share
	// a dispatch command, such as LSP workspace symbols, are distinguished by Query.
	Command     string               `json:"command"`
	Query       map[string]string    `json:"query,omitempty"`
	Empty       bool                 `json:"empty"`
	Message     string               `json:"message,omitempty"`
	Summary     *codeIntelSummary    `json:"summary,omitempty"`
	Files       []codeIntelFile      `json:"files,omitempty"`
	Packages    []codeIntelPackage   `json:"packages,omitempty"`
	Symbols     []codeIntelSymbol    `json:"symbols,omitempty"`
	Imports     []codeIntelImport    `json:"imports,omitempty"`
	Edges       []codeIntelEdge      `json:"edges,omitempty"`
	ImpactSet   []codeIntelNode      `json:"impact_set,omitempty"`
	Nodes       []codeIntelNode      `json:"nodes,omitempty"`
	Cycles      []codeIntelCycle     `json:"cycles,omitempty"`
	Layers      []codeIntelLayer     `json:"layers,omitempty"`
	LSPSymbols  []codeIntelLSPSymbol `json:"lsp_symbols,omitempty"`
	Records     []codeIntelRecord    `json:"records,omitempty"`
	Uncertainty []string             `json:"uncertainty,omitempty"`
	Pagination  *codeIntelPagination `json:"pagination,omitempty"`
	TextKind    codeIntelTextKind    `json:"-"`
}

type codeIntelSummary struct {
	Files    int `json:"files"`
	Packages int `json:"packages"`
	Symbols  int `json:"symbols"`
	Imports  int `json:"imports"`
	Nodes    int `json:"nodes"`
	Edges    int `json:"edges"`
	Cycles   int `json:"cycles"`
	Layers   int `json:"layers"`
}

type codeIntelPagination struct {
	Limit    *int `json:"limit,omitempty"`
	Offset   int  `json:"offset"`
	Total    int  `json:"total"`
	Returned int  `json:"returned"`
}

type codeIntelFile struct {
	Path        string            `json:"path"`
	Package     string            `json:"package,omitempty"`
	ImportCount *int              `json:"import_count,omitempty"`
	SymbolCount *int              `json:"symbol_count,omitempty"`
	Imports     []string          `json:"imports,omitempty"`
	Symbols     []codeIntelSymbol `json:"symbols,omitempty"`
}

//nolint:govet // JSON field order groups package identity before optional count pointers.
type codeIntelPackage struct {
	Name          string `json:"name"`
	Files         *int   `json:"files,omitempty"`
	Symbols       *int   `json:"symbols,omitempty"`
	Imports       *int   `json:"imports,omitempty"`
	UniqueImports *int   `json:"unique_imports,omitempty"`
}

type codeIntelSymbol struct {
	Name  string `json:"name,omitempty"`
	Kind  string `json:"kind,omitempty"`
	Path  string `json:"path,omitempty"`
	Line  int    `json:"line,omitempty"`
	Count int    `json:"count,omitempty"`
}

type codeIntelImport struct {
	Path  string `json:"path"`
	Files int    `json:"files,omitempty"`
}

type codeIntelEdge struct {
	Path   string `json:"path"`
	Import string `json:"import"`
}

type codeIntelNode struct {
	Path string `json:"path"`
}

//nolint:govet // JSON field order mirrors text output.
type codeIntelCycle struct {
	Index int      `json:"index"`
	Nodes []string `json:"nodes"`
}

//nolint:govet // JSON field order mirrors text output.
type codeIntelLayer struct {
	Index int      `json:"index"`
	Nodes []string `json:"nodes"`
}

//nolint:govet // JSON field order mirrors LSP output shape.
type codeIntelLSPSymbol struct {
	Name           string               `json:"name"`
	Kind           int                  `json:"kind"`
	Detail         string               `json:"detail,omitempty"`
	Container      string               `json:"container,omitempty"`
	URI            string               `json:"uri,omitempty"`
	Range          codeIntelLSPRange    `json:"range"`
	SelectionRange codeIntelLSPRange    `json:"selection_range"`
	Children       []codeIntelLSPSymbol `json:"children,omitempty"`
}

//nolint:govet // JSON field order keeps common record identity before optional details.
type codeIntelRecord struct {
	Type             string `json:"type"`
	Language         string `json:"language,omitempty"`
	Name             string `json:"name,omitempty"`
	Kind             string `json:"kind,omitempty"`
	Path             string `json:"path,omitempty"`
	Line             int    `json:"line,omitempty"`
	Column           int    `json:"column,omitempty"`
	EndLine          int    `json:"end_line,omitempty"`
	EndColumn        int    `json:"end_column,omitempty"`
	ID               string `json:"id,omitempty"`
	Source           string `json:"source,omitempty"`
	FromID           string `json:"from_id,omitempty"`
	ToID             string `json:"to_id,omitempty"`
	RelationshipKind string `json:"relationship_kind,omitempty"`
	Severity         string `json:"severity,omitempty"`
	Message          string `json:"message,omitempty"`
}

type codeIntelLSPPosition struct {
	Line      int `json:"line"`
	Character int `json:"character"`
}

type codeIntelLSPRange struct {
	Start codeIntelLSPPosition `json:"start"`
	End   codeIntelLSPPosition `json:"end"`
}

func codeIntelOutputFormat(input codeIntelCommandInput) (string, error) {
	return structuredCommandOutputFormat(input.JSON, input.OutputFormat)
}

func structuredCommandOutputFormat(jsonOutput bool, outputFormat string) (string, error) {
	format, err := normalizeOutputFormat(outputFormat)
	if err != nil {
		return "", err
	}

	if jsonOutput {
		return outputFormatJSON, nil
	}

	return format, nil
}

func newCodeIntelResponse(commandName string) codeIntelResponse {
	return codeIntelResponse{Schema: codeIntelSchemaVersion, Command: commandName}
}

func finalizeCodeIntelResponse(response codeIntelResponse) codeIntelResponse {
	if responseHasData(response) {
		response.Empty = false
		response.Message = ""

		return response
	}

	response.Empty = true
	response.Message = codeIntelEmptyMessage

	return response
}

func responseHasData(response codeIntelResponse) bool {
	return response.Summary != nil || len(response.Files) > 0 || len(response.Packages) > 0 || len(response.Symbols) > 0 ||
		len(response.Imports) > 0 || len(response.Edges) > 0 || len(response.ImpactSet) > 0 || len(response.Nodes) > 0 ||
		len(response.Cycles) > 0 || len(response.Layers) > 0 ||
		len(response.LSPSymbols) > 0 || len(response.Records) > 0
}

func codeIntelPayloadFieldForKind(kind codeIntelTextKind) (string, bool) {
	switch kind {
	case codeIntelTextSummary:
		return "summary", true
	case codeIntelTextFiles, codeIntelTextFileDetail, codeIntelTextSymbolFileSummary, codeIntelTextImportFileSummary:
		return "files", true
	case codeIntelTextSymbols, codeIntelTextFileSymbols, codeIntelTextSymbolSummary:
		return "symbols", true
	case codeIntelTextPackages, codeIntelTextPackageImportSummary, codeIntelTextPackageImportMatchSummary:
		return "packages", true
	case codeIntelTextImports, codeIntelTextImportSummary:
		return "imports", true
	case codeIntelTextEdges:
		return "edges", true
	case codeIntelTextImpactSet:
		return "impact_set", true
	case codeIntelTextGraphNodes:
		return "nodes", true
	case codeIntelTextCycles:
		return "cycles", true
	case codeIntelTextLayers:
		return "layers", true
	case codeIntelTextLSPSymbols:
		return "lsp_symbols", true
	case codeIntelTextQuery:
		return "records", true
	default:
		return "", false
	}
}

func codeIntelCountValue(value *int) int {
	if value == nil {
		return 0
	}

	return *value
}
