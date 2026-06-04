package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tommoulard/atteler/pkg/codeintel"
	"github.com/tommoulard/atteler/pkg/lsp"
	"github.com/tommoulard/atteler/pkg/permission"
)

func TestCodeIntelResponse_JSONSchemaForSymbol(t *testing.T) {
	t.Parallel()

	root := writeCodeIntelSchemaFixture(t)
	response, err := buildCodeIntelResponse(t.Context(), root, codeIntelCommandInput{SymbolName: "Run"}, "code-symbol-name")
	require.NoError(t, err)

	var out bytes.Buffer
	require.NoError(t, writeCodeIntelResponse(&out, response, outputFormatJSON))

	assert.JSONEq(t, `{
		"schema": "atteler.code_intel.v1",
		"command": "code-symbol-name",
		"query": {"symbol": "Run"},
		"empty": false,
		"symbols": [{"name": "Run", "kind": "func", "path": "runner.go", "line": 3}]
	}`, out.String())
}

func TestCodeIntelResponse_TextRendersFromSchema(t *testing.T) {
	t.Parallel()

	root := writeCodeIntelSchemaFixture(t)
	response, err := buildCodeIntelResponse(t.Context(), root, codeIntelCommandInput{SymbolName: "Run"}, "code-symbol-name")
	require.NoError(t, err)

	var out bytes.Buffer
	require.NoError(t, writeCodeIntelResponse(&out, response, outputFormatText))

	assert.Equal(t, "Run\tkind=func\tpath=runner.go\tline=3\n", out.String())
}

func TestCodeIntelResponse_FileDetailTextKeepsDocumentedOrder(t *testing.T) {
	t.Parallel()

	response := codeIntelResponse{
		Schema:   codeIntelSchemaVersion,
		Command:  "code-file-path",
		TextKind: codeIntelTextFileDetail,
		Files: []codeIntelFile{{
			Path:        "runner.go",
			Package:     "main",
			ImportCount: new(1),
			SymbolCount: new(1),
			Imports:     []string{"context"},
			Symbols:     []codeIntelSymbol{{Name: "Run", Kind: "func", Line: 3}},
		}},
	}

	var out bytes.Buffer
	require.NoError(t, writeCodeIntelResponse(&out, response, outputFormatText))

	assert.Equal(t, "path=runner.go\tpackage=main\timports=1\tsymbols=1\nimports:\n  - context\nsymbols:\n  - Run\tkind=func\tline=3\n", out.String())
}

func TestCodeIntelResponse_TextRendererCoversDocumentedKinds(t *testing.T) {
	t.Parallel()

	tests := []struct {
		response codeIntelResponse
		kind     codeIntelTextKind
		want     string
	}{
		{
			kind:     codeIntelTextSummary,
			response: codeIntelResponse{Summary: &codeIntelSummary{Files: 1, Packages: 1, Symbols: 2, Imports: 3, Nodes: 4, Edges: 5, Cycles: 6, Layers: 7}},
			want:     "files=1\tpackages=1\tsymbols=2\timports=3\tnodes=4\tedges=5\tcycles=6\tlayers=7\n",
		},
		{
			kind: codeIntelTextFiles,
			response: codeIntelResponse{Files: []codeIntelFile{{
				Path: "runner.go", Package: "main", SymbolCount: new(2), ImportCount: new(1),
			}}},
			want: "path=runner.go\tpackage=main\tsymbols=2\timports=1\n",
		},
		{
			kind: codeIntelTextFileDetail,
			response: codeIntelResponse{Files: []codeIntelFile{{
				Path:        "runner.go",
				Package:     "main",
				SymbolCount: new(1),
				ImportCount: new(1),
				Imports:     []string{"context"},
				Symbols:     []codeIntelSymbol{{Name: "Run", Kind: "func", Line: 3}},
			}}},
			want: "path=runner.go\tpackage=main\timports=1\tsymbols=1\nimports:\n  - context\nsymbols:\n  - Run\tkind=func\tline=3\n",
		},
		{
			kind:     codeIntelTextSymbols,
			response: codeIntelResponse{Symbols: []codeIntelSymbol{{Name: "Run", Kind: "func", Path: "runner.go", Line: 3}}},
			want:     "Run\tkind=func\tpath=runner.go\tline=3\n",
		},
		{
			kind:     codeIntelTextFileSymbols,
			response: codeIntelResponse{Symbols: []codeIntelSymbol{{Name: "Run", Kind: "func", Line: 3}}},
			want:     "Run\tkind=func\tline=3\n",
		},
		{
			kind:     codeIntelTextSymbolSummary,
			response: codeIntelResponse{Symbols: []codeIntelSymbol{{Kind: "func", Count: 2}}},
			want:     "kind=func\tsymbols=2\n",
		},
		{
			kind: codeIntelTextSymbolFileSummary,
			response: codeIntelResponse{Files: []codeIntelFile{{
				Path: "runner.go", Package: "main", SymbolCount: new(2),
			}}},
			want: "path=runner.go\tpackage=main\tsymbols=2\n",
		},
		{
			kind: codeIntelTextPackages,
			response: codeIntelResponse{Packages: []codeIntelPackage{{
				Name: "main", Files: new(1), Symbols: new(2),
			}}},
			want: "package=main\tfiles=1\tsymbols=2\n",
		},
		{
			kind:     codeIntelTextImports,
			response: codeIntelResponse{Imports: []codeIntelImport{{Path: "context"}}},
			want:     "import=context\n",
		},
		{
			kind:     codeIntelTextImportSummary,
			response: codeIntelResponse{Imports: []codeIntelImport{{Path: "context", Files: 1}}},
			want:     "import=context\tfiles=1\n",
		},
		{
			kind: codeIntelTextImportFileSummary,
			response: codeIntelResponse{Files: []codeIntelFile{{
				Path: "runner.go", Package: "main", ImportCount: new(1),
			}}},
			want: "path=runner.go\tpackage=main\timports=1\n",
		},
		{
			kind: codeIntelTextPackageImportSummary,
			response: codeIntelResponse{Packages: []codeIntelPackage{{
				Name: "main", Files: new(1), Imports: new(2), UniqueImports: new(1),
			}}},
			want: "package=main\tfiles=1\timports=2\tunique_imports=1\n",
		},
		{
			kind: codeIntelTextPackageImportMatchSummary,
			response: codeIntelResponse{Packages: []codeIntelPackage{{
				Name: "main", Files: new(1), Imports: new(2),
			}}},
			want: "package=main\tfiles=1\timports=2\n",
		},
		{
			kind:     codeIntelTextEdges,
			response: codeIntelResponse{Edges: []codeIntelEdge{{Path: "runner.go", Import: "context"}}},
			want:     "path=runner.go\timport=context\n",
		},
		{
			kind:     codeIntelTextImpactSet,
			response: codeIntelResponse{ImpactSet: []codeIntelNode{{Path: "runner.go"}}},
			want:     "path=runner.go\n",
		},
		{
			kind:     codeIntelTextGraphNodes,
			response: codeIntelResponse{Nodes: []codeIntelNode{{Path: "context"}}},
			want:     "node=context\n",
		},
		{
			kind:     codeIntelTextCycles,
			response: codeIntelResponse{Cycles: []codeIntelCycle{{Index: 1, Nodes: []string{"a.go", "b.go"}}}},
			want:     "cycle=1\tnodes=a.go -> b.go\n",
		},
		{
			kind:     codeIntelTextLayers,
			response: codeIntelResponse{Layers: []codeIntelLayer{{Index: 1, Nodes: []string{"a.go", "b.go"}}}},
			want:     "layer=1\tnodes=a.go,b.go\n",
		},
		{
			kind: codeIntelTextLSPSymbols,
			response: codeIntelResponse{LSPSymbols: []codeIntelLSPSymbol{{
				Name: "Handle", Kind: 12, Detail: "func()", Container: "server", URI: "file:///repo/main.go",
				Range: codeIntelLSPRange{Start: codeIntelLSPPosition{Line: 2, Character: 1}, End: codeIntelLSPPosition{Line: 4, Character: 2}},
			}}},
			want: "Handle\tkind=12\trange=2:1-4:2\tdetail=func()\tcontainer=server\turi=file:///repo/main.go\n",
		},
		{
			kind: codeIntelTextQuery,
			response: codeIntelResponse{Records: []codeIntelRecord{{
				Type: "definition", Language: "python", Name: "helper", Kind: "function", Path: "worker.py", Line: 3, Column: 5,
			}}},
			want: "type=definition\tlanguage=python\tname=helper\tkind=function\tpath=worker.py\tline=3\tcolumn=5\n",
		},
	}

	for _, test := range tests {
		t.Run(string(test.kind), func(t *testing.T) {
			t.Parallel()

			test.response.Schema = codeIntelSchemaVersion
			test.response.Command = string(test.kind)
			test.response.TextKind = test.kind

			var out bytes.Buffer
			require.NoError(t, writeCodeIntelResponse(&out, test.response, outputFormatText))
			assert.Equal(t, test.want, out.String())
		})
	}
}

func TestCodeIntelResponse_TextRendererReportsMalformedPayloads(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		kind    codeIntelTextKind
		payload codeIntelResponse
		wantErr string
	}{
		{
			name: "summary without summary payload",
			kind: codeIntelTextSummary,
			payload: codeIntelResponse{
				Files: []codeIntelFile{{Path: "runner.go"}},
			},
			wantErr: `code-intel text renderer "summary" requires summary payload`,
		},
		{
			name: "file detail without file payload",
			kind: codeIntelTextFileDetail,
			payload: codeIntelResponse{
				Summary: &codeIntelSummary{Files: 1},
			},
			wantErr: `code-intel text renderer "file_detail" requires files payload`,
		},
		{
			name: "symbols without symbols payload",
			kind: codeIntelTextSymbols,
			payload: codeIntelResponse{
				Files: []codeIntelFile{{Path: "runner.go"}},
			},
			wantErr: `code-intel text renderer "symbols" requires symbols payload`,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			test.payload.Schema = codeIntelSchemaVersion
			test.payload.Command = string(test.kind)
			test.payload.TextKind = test.kind

			var out bytes.Buffer

			err := writeCodeIntelResponse(&out, test.payload, outputFormatText)

			require.Error(t, err)
			require.ErrorContains(t, err, test.wantErr)
			assert.Empty(t, out.String())
		})
	}
}

func TestCodeIntelResponse_RendererRejectsUnsupportedFormatsAndTextKinds(t *testing.T) {
	t.Parallel()

	t.Run("unsupported output format", func(t *testing.T) {
		t.Parallel()

		var out bytes.Buffer

		err := writeCodeIntelResponse(&out, codeIntelResponse{
			Schema:   codeIntelSchemaVersion,
			Command:  codeIntelSummaryCommandName,
			Summary:  &codeIntelSummary{Files: 1},
			TextKind: codeIntelTextSummary,
		}, "yaml")

		require.EqualError(t, err, `unsupported code-intel output format "yaml"`)
		assert.Empty(t, out.String())
	})

	t.Run("unsupported text kind", func(t *testing.T) {
		t.Parallel()

		var out bytes.Buffer

		err := writeCodeIntelResponse(&out, codeIntelResponse{
			Schema:   codeIntelSchemaVersion,
			Command:  "unknown",
			Summary:  &codeIntelSummary{Files: 1},
			TextKind: "unknown",
		}, outputFormatText)

		require.EqualError(t, err, `unsupported code-intel text renderer "unknown"`)
		assert.Empty(t, out.String())
	})
}

func TestCodeIntelTextPayloadValidationCoversRegisteredKinds(t *testing.T) {
	t.Parallel()

	registeredKinds := map[codeIntelTextKind]bool{
		codeIntelTextLSPSymbols: true,
	}
	for _, descriptor := range codeIntelCommandDescriptors() {
		registeredKinds[descriptor.TextKind] = true
	}

	for kind := range registeredKinds {
		t.Run(string(kind), func(t *testing.T) {
			t.Parallel()

			field, _, known := codeIntelTextPayloadPresent(codeIntelResponse{TextKind: kind})
			payloadField, payloadKnown := codeIntelPayloadFieldForKind(kind)

			assert.True(t, known, "text kind %s should have payload validation", kind)
			assert.NotEmpty(t, field, "text kind %s should report a payload field", kind)
			assert.True(t, payloadKnown, "text kind %s should have a schema payload field", kind)
			assert.Equal(t, payloadField, field, "text kind %s should validate the documented payload field", kind)
		})
	}

	field, present, known := codeIntelTextPayloadPresent(codeIntelResponse{TextKind: "unknown"})
	assert.False(t, known)
	assert.False(t, present)
	assert.Empty(t, field)
	field, known = codeIntelPayloadFieldForKind("unknown")
	assert.False(t, known)
	assert.Empty(t, field)
}

func TestCodeIntelResponse_PackageCountsIncludeRelevantZeroes(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, "empty.go"), []byte("package empty\n"), 0o600))

	response, err := buildCodeIntelResponse(t.Context(), root, codeIntelCommandInput{ListPackages: true}, "list-code-packages")
	require.NoError(t, err)

	var out bytes.Buffer
	require.NoError(t, writeCodeIntelResponse(&out, response, outputFormatJSON))

	assert.JSONEq(t, `{
		"schema": "atteler.code_intel.v1",
		"command": "list-code-packages",
		"empty": false,
		"packages": [{"name": "empty", "files": 1, "symbols": 0}]
	}`, out.String())
}

func TestCodeIntelResponse_ImpactSetUsesDedicatedSchemaField(t *testing.T) {
	t.Parallel()

	root := writeCodeIntelFullSchemaFixture(t)
	response, err := buildCodeIntelResponse(t.Context(), root, codeIntelCommandInput{ImpactTarget: "context"}, "code-impact-target")
	require.NoError(t, err)

	assert.False(t, response.Empty)
	assert.Equal(t, codeIntelTextImpactSet, response.TextKind)
	require.Len(t, response.ImpactSet, 1)
	assert.Empty(t, response.Nodes)

	var text bytes.Buffer
	require.NoError(t, writeCodeIntelResponse(&text, response, outputFormatText))
	assert.Equal(t, "path=runner.go\n", text.String())

	var jsonOut bytes.Buffer
	require.NoError(t, writeCodeIntelResponse(&jsonOut, response, outputFormatJSON))
	assert.JSONEq(t, `{
		"schema": "atteler.code_intel.v1",
		"command": "code-impact-target",
		"query": {"target": "context"},
		"empty": false,
		"impact_set": [{"path": "runner.go"}]
	}`, jsonOut.String())
}

func TestCodeIntelResponse_EmptyStateIsConsistent(t *testing.T) {
	t.Parallel()

	root := writeCodeIntelSchemaFixture(t)
	response, err := buildCodeIntelResponse(t.Context(), root, codeIntelCommandInput{SymbolName: "Missing"}, "code-symbol-name")
	require.NoError(t, err)

	assert.True(t, response.Empty)
	assert.Equal(t, codeIntelEmptyMessage, response.Message)

	var text bytes.Buffer
	require.NoError(t, writeCodeIntelResponse(&text, response, outputFormatText))
	assert.Equal(t, codeIntelEmptyMessage+"\n", text.String())

	var jsonOut bytes.Buffer
	require.NoError(t, writeCodeIntelResponse(&jsonOut, response, outputFormatJSON))
	assert.JSONEq(t, `{
		"schema": "atteler.code_intel.v1",
		"command": "code-symbol-name",
		"query": {"symbol": "Missing"},
		"empty": true,
		"message": "No code-intel results found."
	}`, jsonOut.String())
}

func TestCodeIntelResponse_FinalizeClearsStaleEmptyStateWhenDataExists(t *testing.T) {
	t.Parallel()

	response := finalizeCodeIntelResponse(codeIntelResponse{
		Schema:   codeIntelSchemaVersion,
		Command:  "code-symbol-name",
		Empty:    true,
		Message:  codeIntelEmptyMessage,
		TextKind: codeIntelTextSymbols,
		Symbols: []codeIntelSymbol{{
			Name: "Run",
			Kind: "func",
			Path: "runner.go",
			Line: 3,
		}},
	})

	assert.False(t, response.Empty)
	assert.Empty(t, response.Message)

	var text bytes.Buffer
	require.NoError(t, writeCodeIntelResponse(&text, response, outputFormatText))
	assert.Equal(t, "Run\tkind=func\tpath=runner.go\tline=3\n", text.String())

	var jsonOut bytes.Buffer
	require.NoError(t, writeCodeIntelResponse(&jsonOut, response, outputFormatJSON))
	assert.JSONEq(t, `{
		"schema": "atteler.code_intel.v1",
		"command": "code-symbol-name",
		"empty": false,
		"symbols": [{"name": "Run", "kind": "func", "path": "runner.go", "line": 3}]
	}`, jsonOut.String())
}

func TestCodeIntelResponse_EmptyStateConsistentAcrossQueryTypes(t *testing.T) {
	t.Parallel()

	root := writeCodeIntelSchemaFixture(t)
	tests := []struct {
		name      string
		wantQuery map[string]string
		input     codeIntelCommandInput
	}{
		{
			name:      "code-symbol-name",
			input:     codeIntelCommandInput{SymbolName: "Missing"},
			wantQuery: map[string]string{"symbol": "Missing"},
		},
		{
			name:      "code-import-path",
			input:     codeIntelCommandInput{ImportPath: "example.com/missing"},
			wantQuery: map[string]string{"import": "example.com/missing"},
		},
		{
			name:      "code-package-name",
			input:     codeIntelCommandInput{PackageName: "missingpkg"},
			wantQuery: map[string]string{"package": "missingpkg"},
		},
		{
			name:      "code-file-path",
			input:     codeIntelCommandInput{FilePath: "missing.go"},
			wantQuery: map[string]string{"path": "missing.go"},
		},
		{
			name:      "code-deps-target",
			input:     codeIntelCommandInput{DepsTarget: "missing.go"},
			wantQuery: map[string]string{"target": "missing.go"},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			response, err := buildCodeIntelResponse(t.Context(), root, test.input, test.name)
			require.NoError(t, err)
			assert.True(t, response.Empty)
			assert.Equal(t, codeIntelEmptyMessage, response.Message)

			var text bytes.Buffer
			require.NoError(t, writeCodeIntelResponse(&text, response, outputFormatText))
			assert.Equal(t, codeIntelEmptyMessage+"\n", text.String())

			var jsonOut bytes.Buffer
			require.NoError(t, writeCodeIntelResponse(&jsonOut, response, outputFormatJSON))

			var decoded codeIntelResponse
			require.NoError(t, json.Unmarshal(jsonOut.Bytes(), &decoded))
			assert.Equal(t, codeIntelSchemaVersion, decoded.Schema)
			assert.Equal(t, test.name, decoded.Command)
			assert.True(t, decoded.Empty)
			assert.Equal(t, codeIntelEmptyMessage, decoded.Message)
			assert.Equal(t, test.wantQuery, decoded.Query)
		})
	}
}

func TestCodeIntelResponse_EmptyStateConsistentForEveryDescriptor(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	inputs := codeIntelTestInputsByCommand()

	for _, descriptor := range codeIntelCommandDescriptors() {
		if descriptor.Name == codeIntelSummaryCommandName {
			continue
		}

		t.Run(descriptor.Name, func(t *testing.T) {
			t.Parallel()

			input, ok := inputs[descriptor.Name]
			require.Truef(t, ok, "missing test input for %s", descriptor.Name)

			response, err := buildCodeIntelResponse(t.Context(), root, input, descriptor.Name)
			require.NoError(t, err)
			assert.True(t, response.Empty)
			assert.Equal(t, codeIntelEmptyMessage, response.Message)

			var text bytes.Buffer
			require.NoError(t, writeCodeIntelResponse(&text, response, outputFormatText))
			assert.Equal(t, codeIntelEmptyMessage+"\n", text.String())

			var jsonOut bytes.Buffer
			require.NoError(t, writeCodeIntelResponse(&jsonOut, response, outputFormatJSON))

			var decoded codeIntelResponse
			require.NoError(t, json.Unmarshal(jsonOut.Bytes(), &decoded))
			assert.Equal(t, codeIntelSchemaVersion, decoded.Schema)
			assert.Equal(t, descriptor.Name, decoded.Command)
			assert.True(t, decoded.Empty)
			assert.Equal(t, codeIntelEmptyMessage, decoded.Message)
		})
	}
}

func TestCodeIntelResponse_PaginatesRepeatedResults(t *testing.T) {
	t.Parallel()

	root := writeCodeIntelFullSchemaFixture(t)
	response, err := buildCodeIntelResponse(t.Context(), root, codeIntelCommandInput{
		ListSymbolSummary: true,
		Limit:             1,
		Offset:            1,
	}, "list-code-symbol-summary")
	require.NoError(t, err)

	require.Len(t, response.Symbols, 1)
	require.NotNil(t, response.Pagination)
	assert.Equal(t, 1, *response.Pagination.Limit)
	assert.Equal(t, 1, response.Pagination.Offset)
	assert.Equal(t, 3, response.Pagination.Total)
	assert.Equal(t, 1, response.Pagination.Returned)

	var text bytes.Buffer
	require.NoError(t, writeCodeIntelResponse(&text, response, outputFormatText))
	assert.Equal(t, "kind=func\tsymbols=1\n", text.String())

	var jsonOut bytes.Buffer
	require.NoError(t, writeCodeIntelResponse(&jsonOut, response, outputFormatJSON))
	assert.JSONEq(t, `{
		"schema": "atteler.code_intel.v1",
		"command": "list-code-symbol-summary",
		"empty": false,
		"symbols": [{"kind": "func", "count": 1}],
		"pagination": {"limit": 1, "offset": 1, "total": 3, "returned": 1}
	}`, jsonOut.String())
}

func TestCodeIntelResponse_EmptyPageKeepsPaginationMetadata(t *testing.T) {
	t.Parallel()

	root := writeCodeIntelFullSchemaFixture(t)
	response, err := buildCodeIntelResponse(t.Context(), root, codeIntelCommandInput{
		ListSymbolSummary: true,
		Offset:            10,
	}, "list-code-symbol-summary")
	require.NoError(t, err)

	assert.True(t, response.Empty)
	assert.Equal(t, codeIntelEmptyMessage, response.Message)
	require.NotNil(t, response.Pagination)
	assert.Nil(t, response.Pagination.Limit)
	assert.Equal(t, 10, response.Pagination.Offset)
	assert.Equal(t, 3, response.Pagination.Total)
	assert.Zero(t, response.Pagination.Returned)

	var jsonOut bytes.Buffer
	require.NoError(t, writeCodeIntelResponse(&jsonOut, response, outputFormatJSON))
	assert.JSONEq(t, `{
		"schema": "atteler.code_intel.v1",
		"command": "list-code-symbol-summary",
		"empty": true,
		"message": "No code-intel results found.",
		"pagination": {"offset": 10, "total": 3, "returned": 0}
	}`, jsonOut.String())
}

func TestCodeIntelResponse_NormalizesNegativePaginationOffset(t *testing.T) {
	t.Parallel()

	root := writeCodeIntelFullSchemaFixture(t)
	response, err := buildCodeIntelResponse(t.Context(), root, codeIntelCommandInput{
		ListSymbolSummary: true,
		Limit:             1,
		Offset:            -10,
	}, "list-code-symbol-summary")
	require.NoError(t, err)

	require.Len(t, response.Symbols, 1)
	require.NotNil(t, response.Pagination)
	assert.Equal(t, 1, *response.Pagination.Limit)
	assert.Zero(t, response.Pagination.Offset)
	assert.Equal(t, 3, response.Pagination.Total)
	assert.Equal(t, 1, response.Pagination.Returned)
}

func TestCodeIntelResponse_EmptyPaginatedListKeepsPaginationMetadata(t *testing.T) {
	t.Parallel()

	root := writeCodeIntelFullSchemaFixture(t)
	response, err := buildCodeIntelResponse(t.Context(), root, codeIntelCommandInput{
		SymbolName: "Missing",
		Limit:      10,
	}, "code-symbol-name")
	require.NoError(t, err)

	assert.True(t, response.Empty)
	assert.Equal(t, codeIntelEmptyMessage, response.Message)
	require.NotNil(t, response.Pagination)
	assert.Equal(t, 10, *response.Pagination.Limit)
	assert.Zero(t, response.Pagination.Offset)
	assert.Zero(t, response.Pagination.Total)
	assert.Zero(t, response.Pagination.Returned)

	var jsonOut bytes.Buffer
	require.NoError(t, writeCodeIntelResponse(&jsonOut, response, outputFormatJSON))
	assert.JSONEq(t, `{
		"schema": "atteler.code_intel.v1",
		"command": "code-symbol-name",
		"query": {"symbol": "Missing"},
		"empty": true,
		"message": "No code-intel results found.",
		"pagination": {"limit": 10, "offset": 0, "total": 0, "returned": 0}
	}`, jsonOut.String())
}

func TestCodeIntelResponse_SummaryIgnoresPaginationFlags(t *testing.T) {
	t.Parallel()

	root := writeCodeIntelFullSchemaFixture(t)
	response, err := buildCodeIntelResponse(t.Context(), root, codeIntelCommandInput{
		Summary: true,
		Limit:   1,
		Offset:  1,
	}, "code-summary")
	require.NoError(t, err)

	assert.NotNil(t, response.Summary)
	assert.Nil(t, response.Pagination)
	assert.NotContains(t, codeIntelJSONFieldsForKind(codeIntelTextSummary), "pagination")
	assert.NotContains(t, codeIntelJSONFieldsForKind(codeIntelTextFileDetail), "pagination")
	assert.NotContains(t, codeIntelJSONFieldsForKind(codeIntelTextLSPSymbols), "pagination")
	assert.Contains(t, codeIntelJSONFieldsForKind(codeIntelTextSymbols), "pagination")
}

func TestCodeIntelResponse_FileDetailIgnoresPaginationFlags(t *testing.T) {
	t.Parallel()

	root := writeCodeIntelFullSchemaFixture(t)
	response, err := buildCodeIntelResponse(t.Context(), root, codeIntelCommandInput{
		FilePath: "runner.go",
		Offset:   1,
	}, "code-file-path")
	require.NoError(t, err)

	assert.False(t, response.Empty)
	assert.Nil(t, response.Pagination)
	require.Len(t, response.Files, 1)
	assert.Equal(t, "runner.go", response.Files[0].Path)
}

func TestCodeIntelResponse_LSPIgnoresPaginationFlags(t *testing.T) {
	t.Parallel()

	response := paginateCodeIntelResponse(codeIntelGoldenLSPResponse(), codeIntelCommandInput{
		Limit:  1,
		Offset: 1,
	})

	assert.Nil(t, response.Pagination)
	require.Len(t, response.LSPSymbols, 1)
	require.Len(t, response.LSPSymbols[0].Children, 1)
	assert.NotContains(t, codeIntelJSONFieldsForKind(codeIntelTextLSPSymbols), "pagination")
}

func TestCodeIntelResponse_QueryCommandUsesSharedWorkspaceIndex(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, "worker.py"), []byte("def helper():\n\treturn 'ok'\n"), 0o600))
	response, err := buildCodeIntelResponse(t.Context(), root, codeIntelCommandInput{
		ModelQuery:    "Definitions:helper",
		ModelLanguage: "Python",
		Limit:         1,
	}, codeIntelQueryCommandName)
	require.NoError(t, err)

	assert.False(t, response.Empty)
	assert.Equal(t, map[string]string{"kind": "definitions", "language": "python", "value": "helper"}, response.Query)
	require.Len(t, response.Records, 1)
	assert.Equal(t, "definition", response.Records[0].Type)
	assert.Equal(t, "python", response.Records[0].Language)
	assert.Equal(t, "helper", response.Records[0].Name)
	assert.Equal(t, "worker.py", response.Records[0].Path)
	require.NotNil(t, response.Pagination)
	assert.Equal(t, 1, *response.Pagination.Limit)
	assert.FileExists(t, filepath.Join(root, ".atteler", "codeintel-index.json"))
}

func TestCodeIntelResponse_QueryCommandFiltersFilesByRelativePath(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(root, "nested"), 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(root, "nested", "worker.py"), []byte("def helper():\n\treturn 'ok'\n"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(root, "other.py"), []byte("def other():\n\treturn 'ok'\n"), 0o600))

	response, err := buildCodeIntelResponse(t.Context(), root, codeIntelCommandInput{
		ModelQuery:    "files:nested/worker.py",
		ModelLanguage: "python",
	}, codeIntelQueryCommandName)
	require.NoError(t, err)

	assert.False(t, response.Empty)
	assert.Equal(t, map[string]string{"kind": "files", "language": "python", "value": "nested/worker.py"}, response.Query)
	require.Len(t, response.Records, 1)
	assert.Equal(t, "file", response.Records[0].Type)
	assert.Equal(t, "nested/worker.py", response.Records[0].Path)
}

func TestCodeIntelModelQueryFromInputNormalizesAliases(t *testing.T) {
	t.Parallel()

	tests := []struct {
		wantValues map[string]string
		wantQuery  codeintel.Query
		name       string
		input      codeIntelCommandInput
	}{
		{
			name:       "definitions alias",
			input:      codeIntelCommandInput{ModelQuery: "defs:Run", ModelLanguage: "Go"},
			wantQuery:  codeintel.Query{Kind: codeintel.QueryDefinitions, Name: "Run", Language: "go"},
			wantValues: map[string]string{"kind": "definitions", "language": "go", "value": "Run"},
		},
		{
			name:       "references alias",
			input:      codeIntelCommandInput{ModelQuery: "refs:Runner.Run"},
			wantQuery:  codeintel.Query{Kind: codeintel.QueryReferences, Name: "Runner.Run"},
			wantValues: map[string]string{"kind": "references", "value": "Runner.Run"},
		},
		{
			name:       "diagnostics alias",
			input:      codeIntelCommandInput{ModelQuery: "diags:parse", ModelLanguage: "Python"},
			wantQuery:  codeintel.Query{Kind: codeintel.QueryDiagnostics, Name: "parse", Language: "python"},
			wantValues: map[string]string{"kind": "diagnostics", "language": "python", "value": "parse"},
		},
		{
			name:       "relationships alias",
			input:      codeIntelCommandInput{ModelQuery: "relations:Imports"},
			wantQuery:  codeintel.Query{Kind: codeintel.QueryRelationships, RelationshipKind: "imports"},
			wantValues: map[string]string{"kind": "relationships", "value": "imports"},
		},
		{
			name:       "edges alias",
			input:      codeIntelCommandInput{ModelQuery: "edges:declares"},
			wantQuery:  codeintel.Query{Kind: codeintel.QueryRelationships, RelationshipKind: "declares"},
			wantValues: map[string]string{"kind": "relationships", "value": "declares"},
		},
		{
			name:       "files filter",
			input:      codeIntelCommandInput{ModelQuery: "files:worker.py"},
			wantQuery:  codeintel.Query{Kind: codeintel.QueryFiles, File: "worker.py"},
			wantValues: map[string]string{"kind": "files", "value": "worker.py"},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			query, values, err := codeIntelModelQueryFromInput(test.input)
			require.NoError(t, err)
			assert.Equal(t, test.wantQuery, query)
			assert.Equal(t, test.wantValues, values)
		})
	}
}

func TestCodeIntelModelQueryFromInputRejectsUnsupportedKind(t *testing.T) {
	t.Parallel()

	_, _, err := codeIntelModelQueryFromInput(codeIntelCommandInput{ModelQuery: "unknown:value"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unsupported code-query kind")
}

func TestCodeIntelResponse_RendererFinalizesEmptyState(t *testing.T) {
	t.Parallel()

	response := newCodeIntelResponse("code-symbol-name")
	response.Query = codeIntelQuery("symbol", "Missing")
	response.TextKind = codeIntelTextSymbols

	var text bytes.Buffer
	require.NoError(t, writeCodeIntelResponse(&text, response, outputFormatText))
	assert.Equal(t, codeIntelEmptyMessage+"\n", text.String())

	var jsonOut bytes.Buffer
	require.NoError(t, writeCodeIntelResponse(&jsonOut, response, outputFormatJSON))
	assert.JSONEq(t, `{
		"schema": "atteler.code_intel.v1",
		"command": "code-symbol-name",
		"query": {"symbol": "Missing"},
		"empty": true,
		"message": "No code-intel results found."
	}`, jsonOut.String())
}

func TestCodeIntelLSPTextEmptyUsesCodeIntelEmptyState(t *testing.T) {
	t.Parallel()

	response := buildLSPCodeIntelResponse(lspSymbolsCommandInput{DocumentSymbols: true}, nil)

	var out bytes.Buffer
	require.NoError(t, writeCodeIntelResponse(&out, response, outputFormatText))

	assert.Equal(t, codeIntelEmptyMessage+"\n", out.String())
}

func TestCodeIntelResponse_LSPSymbolsEmptyJSONUsesSchema(t *testing.T) {
	t.Parallel()

	response := codeIntelResponse{
		Schema:   codeIntelSchemaVersion,
		Command:  codeIntelLSPSymbolsName,
		Query:    codeIntelQuery("workspace_symbols", "Missing"),
		TextKind: codeIntelTextLSPSymbols,
	}

	var out bytes.Buffer
	require.NoError(t, writeCodeIntelResponse(&out, response, outputFormatJSON))

	assert.JSONEq(t, `{
		"schema": "atteler.code_intel.v1",
		"command": "lsp-symbols",
		"query": {"workspace_symbols": "Missing"},
		"empty": true,
		"message": "No code-intel results found."
	}`, out.String())
}

func TestCodeIntelResponse_LSPBuilderUsesWorkspaceQueryAndDispatchCommand(t *testing.T) {
	t.Parallel()

	response := buildLSPCodeIntelResponse(lspSymbolsCommandInput{
		Command:          "gopls",
		Args:             []string{"-remote=auto"},
		RootPath:         "/repo",
		LanguageID:       "go",
		WorkspaceSymbols: "Handle",
	}, []lsp.Symbol{{
		Name:           "Handle",
		Kind:           12,
		Range:          lsp.Range{Start: lsp.Position{Line: 2, Character: 1}, End: lsp.Position{Line: 4, Character: 2}},
		SelectionRange: lsp.Range{Start: lsp.Position{Line: 2, Character: 6}, End: lsp.Position{Line: 2, Character: 12}},
	}})

	assert.Equal(t, codeIntelSchemaVersion, response.Schema)
	assert.Equal(t, codeIntelLSPSymbolsName, response.Command)
	assert.Equal(t, codeIntelTextLSPSymbols, response.TextKind)
	assert.False(t, response.Empty)
	assert.Empty(t, response.Message)
	assert.Equal(t, map[string]string{
		"args":              "-remote=auto",
		"command":           "gopls",
		"language":          "go",
		"root":              "/repo",
		"workspace_symbols": "Handle",
	}, response.Query)
	require.Len(t, response.LSPSymbols, 1)
	assert.Equal(t, "Handle", response.LSPSymbols[0].Name)

	var out bytes.Buffer
	require.NoError(t, writeCodeIntelResponse(&out, response, outputFormatJSON))
	assert.JSONEq(t, `{
		"schema": "atteler.code_intel.v1",
		"command": "lsp-symbols",
		"query": {
			"args": "-remote=auto",
			"command": "gopls",
			"language": "go",
			"root": "/repo",
			"workspace_symbols": "Handle"
		},
		"empty": false,
		"lsp_symbols": [{
			"name": "Handle",
			"kind": 12,
			"range": {"start": {"line": 2, "character": 1}, "end": {"line": 4, "character": 2}},
			"selection_range": {"start": {"line": 2, "character": 6}, "end": {"line": 2, "character": 12}}
		}]
	}`, out.String())
}

func TestCodeIntelResponse_LSPSymbolsJSONSchema(t *testing.T) {
	t.Parallel()

	response := codeIntelGoldenLSPResponse()

	var out bytes.Buffer
	require.NoError(t, writeCodeIntelResponse(&out, response, outputFormatJSON))

	assert.JSONEq(t, `{
		"schema": "atteler.code_intel.v1",
		"command": "lsp-symbols",
		"query": {"file": "main.go"},
		"empty": false,
		"lsp_symbols": [{
			"name": "Handle",
			"kind": 12,
			"detail": "func()",
			"container": "server",
			"uri": "file:///repo/main.go",
			"range": {"start": {"line": 2, "character": 1}, "end": {"line": 4, "character": 2}},
			"selection_range": {"start": {"line": 2, "character": 6}, "end": {"line": 2, "character": 12}},
			"children": [{
				"name": "Inner",
				"kind": 5,
				"range": {"start": {"line": 3, "character": 2}, "end": {"line": 3, "character": 8}},
				"selection_range": {"start": {"line": 3, "character": 2}, "end": {"line": 3, "character": 7}}
			}]
		}]
	}`, out.String())
}

func TestCodeIntelResponse_LSPGoldenTextAndJSONOutputs(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		format     string
		goldenName string
	}{
		{name: "text", format: outputFormatText, goldenName: "lsp.text.golden"},
		{name: "json", format: outputFormatJSON, goldenName: "lsp.json.golden"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			response := codeIntelGoldenLSPResponse()

			var out bytes.Buffer
			require.NoError(t, writeCodeIntelResponse(&out, response, test.format))

			assert.Equal(t, readCodeIntelGolden(t, test.goldenName), out.String())
		})
	}
}

func codeIntelGoldenLSPResponse() codeIntelResponse {
	return finalizeCodeIntelResponse(codeIntelResponse{
		Schema:  codeIntelSchemaVersion,
		Command: "lsp-symbols",
		Query:   codeIntelQueryPairs("file", "main.go"),
		LSPSymbols: codeIntelLSPSymbolsFromLSP([]lsp.Symbol{{
			Name:           "Handle",
			Kind:           12,
			Detail:         "func()",
			ContainerName:  "server",
			URI:            "file:///repo/main.go",
			Range:          lsp.Range{Start: lsp.Position{Line: 2, Character: 1}, End: lsp.Position{Line: 4, Character: 2}},
			SelectionRange: lsp.Range{Start: lsp.Position{Line: 2, Character: 6}, End: lsp.Position{Line: 2, Character: 12}},
			Children: []lsp.Symbol{{
				Name:           "Inner",
				Kind:           5,
				Range:          lsp.Range{Start: lsp.Position{Line: 3, Character: 2}, End: lsp.Position{Line: 3, Character: 8}},
				SelectionRange: lsp.Range{Start: lsp.Position{Line: 3, Character: 2}, End: lsp.Position{Line: 3, Character: 7}},
			}},
		}}),
		TextKind: codeIntelTextLSPSymbols,
	})
}

func TestLSPSymbolsResponseCommandUsesRegisteredDispatchName(t *testing.T) {
	t.Parallel()

	assert.Equal(t, "lsp-symbols", lspSymbolsResponseCommand(lspSymbolsCommandInput{DocumentSymbols: true}))
	assert.Equal(t, "lsp-symbols", lspSymbolsResponseCommand(lspSymbolsCommandInput{WorkspaceSymbols: "Handler"}))
}

func TestCodeIntelResponse_GoldenTextAndJSONOutputs(t *testing.T) {
	t.Parallel()

	root := writeCodeIntelSchemaFixture(t)
	fullRoot := writeCodeIntelFullSchemaFixture(t)
	cycleRoot := writeCodeIntelCycleSchemaFixture(t)
	tests := []struct {
		command    string
		format     string
		goldenName string
		root       string
		input      codeIntelCommandInput
	}{
		{
			input:      codeIntelCommandInput{SymbolName: "Run"},
			command:    "code-symbol-name",
			format:     outputFormatText,
			goldenName: "symbol.text.golden",
		},
		{
			input:      codeIntelCommandInput{SymbolName: "Run"},
			command:    "code-symbol-name",
			format:     outputFormatJSON,
			goldenName: "symbol.json.golden",
		},
		{
			input:      codeIntelCommandInput{Summary: true},
			command:    "code-summary",
			format:     outputFormatText,
			goldenName: "summary.text.golden",
		},
		{
			input:      codeIntelCommandInput{Summary: true},
			command:    "code-summary",
			format:     outputFormatJSON,
			goldenName: "summary.json.golden",
		},
		{
			input:      codeIntelCommandInput{FilePath: "runner.go"},
			command:    "code-file-path",
			format:     outputFormatText,
			goldenName: "file.text.golden",
		},
		{
			input:      codeIntelCommandInput{FilePath: "runner.go"},
			command:    "code-file-path",
			format:     outputFormatJSON,
			goldenName: "file.json.golden",
		},
		{
			root:       fullRoot,
			input:      codeIntelCommandInput{ListPackages: true},
			command:    "list-code-packages",
			format:     outputFormatText,
			goldenName: "packages.text.golden",
		},
		{
			root:       fullRoot,
			input:      codeIntelCommandInput{ListPackages: true},
			command:    "list-code-packages",
			format:     outputFormatJSON,
			goldenName: "packages.json.golden",
		},
		{
			root:       fullRoot,
			input:      codeIntelCommandInput{ListImports: true},
			command:    "list-code-imports",
			format:     outputFormatText,
			goldenName: "imports.text.golden",
		},
		{
			root:       fullRoot,
			input:      codeIntelCommandInput{ListImports: true},
			command:    "list-code-imports",
			format:     outputFormatJSON,
			goldenName: "imports.json.golden",
		},
		{
			root:       fullRoot,
			input:      codeIntelCommandInput{ListLayers: true},
			command:    "list-code-layers",
			format:     outputFormatText,
			goldenName: "layers.text.golden",
		},
		{
			root:       fullRoot,
			input:      codeIntelCommandInput{ListLayers: true},
			command:    "list-code-layers",
			format:     outputFormatJSON,
			goldenName: "layers.json.golden",
		},
		{
			root:       fullRoot,
			input:      codeIntelCommandInput{DepsTarget: "runner.go"},
			command:    "code-deps-target",
			format:     outputFormatText,
			goldenName: "nodes.text.golden",
		},
		{
			root:       fullRoot,
			input:      codeIntelCommandInput{DepsTarget: "runner.go"},
			command:    "code-deps-target",
			format:     outputFormatJSON,
			goldenName: "nodes.json.golden",
		},
		{
			root:       fullRoot,
			input:      codeIntelCommandInput{ImpactTarget: "context"},
			command:    "code-impact-target",
			format:     outputFormatText,
			goldenName: "impact.text.golden",
		},
		{
			root:       fullRoot,
			input:      codeIntelCommandInput{ImpactTarget: "context"},
			command:    "code-impact-target",
			format:     outputFormatJSON,
			goldenName: "impact.json.golden",
		},
		{
			root:       cycleRoot,
			input:      codeIntelCommandInput{ListCycles: true},
			command:    codeIntelCyclesCommandName,
			format:     outputFormatText,
			goldenName: "cycles.text.golden",
		},
		{
			root:       cycleRoot,
			input:      codeIntelCommandInput{ListCycles: true},
			command:    codeIntelCyclesCommandName,
			format:     outputFormatJSON,
			goldenName: "cycles.json.golden",
		},
		{
			input:      codeIntelCommandInput{SymbolName: "Missing"},
			command:    "code-symbol-name",
			format:     outputFormatText,
			goldenName: "empty.text.golden",
		},
		{
			input:      codeIntelCommandInput{SymbolName: "Missing"},
			command:    "code-symbol-name",
			format:     outputFormatJSON,
			goldenName: "empty.json.golden",
		},
	}

	for _, test := range tests {
		t.Run(test.goldenName, func(t *testing.T) {
			t.Parallel()

			testRoot := root
			if test.root != "" {
				testRoot = test.root
			}

			response, err := buildCodeIntelResponse(t.Context(), testRoot, test.input, test.command)
			require.NoError(t, err)

			var out bytes.Buffer
			require.NoError(t, writeCodeIntelResponse(&out, response, test.format))

			assert.Equal(t, readCodeIntelGolden(t, test.goldenName), out.String())
		})
	}
}

func TestCodeIntelResponse_FilteredCommandPayloadsAreStable(t *testing.T) {
	t.Parallel()

	root := writeCodeIntelFullSchemaFixture(t)
	tests := []struct {
		name     string
		command  string
		wantText string
		wantJSON string
		input    codeIntelCommandInput
	}{
		{
			name:     "import prefix edges",
			command:  "code-import-prefix",
			input:    codeIntelCommandInput{ImportPrefix: "con"},
			wantText: "path=runner.go\timport=context\n",
			wantJSON: `{
				"schema": "atteler.code_intel.v1",
				"command": "code-import-prefix",
				"query": {"prefix": "con"},
				"empty": false,
				"edges": [{"path": "runner.go", "import": "context"}]
			}`,
		},
		{
			name:     "import prefix file summary",
			command:  "code-import-prefix-file-summary",
			input:    codeIntelCommandInput{ImportPrefixFileSummary: "con"},
			wantText: "path=runner.go\tpackage=main\timports=1\n",
			wantJSON: `{
				"schema": "atteler.code_intel.v1",
				"command": "code-import-prefix-file-summary",
				"query": {"prefix": "con"},
				"empty": false,
				"files": [{"path": "runner.go", "package": "main", "import_count": 1}]
			}`,
		},
		{
			name:     "import prefix package summary",
			command:  "code-import-prefix-package-summary",
			input:    codeIntelCommandInput{ImportPrefixPackageSummary: "con"},
			wantText: "package=main\tfiles=1\timports=1\n",
			wantJSON: `{
				"schema": "atteler.code_intel.v1",
				"command": "code-import-prefix-package-summary",
				"query": {"prefix": "con"},
				"empty": false,
				"packages": [{"name": "main", "files": 1, "imports": 1}]
			}`,
		},
		{
			name:     "package import prefix",
			command:  "code-package-import-prefix",
			input:    codeIntelCommandInput{PackageImportPrefix: "main:con"},
			wantText: "import=context\tfiles=1\n",
			wantJSON: `{
				"schema": "atteler.code_intel.v1",
				"command": "code-package-import-prefix",
				"query": {"package": "main", "prefix": "con"},
				"empty": false,
				"imports": [{"path": "context", "files": 1}]
			}`,
		},
		{
			name:     "file import prefix",
			command:  "code-file-import-prefix",
			input:    codeIntelCommandInput{FileImportPrefix: "runner.go:con"},
			wantText: "import=context\n",
			wantJSON: `{
				"schema": "atteler.code_intel.v1",
				"command": "code-file-import-prefix",
				"query": {"path": "runner.go", "prefix": "con"},
				"empty": false,
				"imports": [{"path": "context"}]
			}`,
		},
		{
			name:     "package symbol kind",
			command:  "code-package-symbol-kind",
			input:    codeIntelCommandInput{PackageSymbolKind: "main:func"},
			wantText: "Run\tkind=func\tpath=runner.go\tline=9\n",
			wantJSON: `{
				"schema": "atteler.code_intel.v1",
				"command": "code-package-symbol-kind",
				"query": {"package": "main", "kind": "func"},
				"empty": false,
				"symbols": [{"name": "Run", "kind": "func", "path": "runner.go", "line": 9}]
			}`,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			response, err := buildCodeIntelResponse(t.Context(), root, test.input, test.command)
			require.NoError(t, err)

			var text bytes.Buffer
			require.NoError(t, writeCodeIntelResponse(&text, response, outputFormatText))
			assert.Equal(t, test.wantText, text.String())

			var jsonOut bytes.Buffer
			require.NoError(t, writeCodeIntelResponse(&jsonOut, response, outputFormatJSON))
			assert.JSONEq(t, test.wantJSON, jsonOut.String())
		})
	}
}

func TestCodeIntelResponse_AllRegisteredCommandsBuildTextAndJSON(t *testing.T) {
	t.Parallel()

	root := writeCodeIntelFullSchemaFixture(t)
	descriptors := codeIntelCommandDescriptorsByName()
	inputs := codeIntelTestInputsByCommand()

	for _, command := range codeIntelCommands() {
		t.Run(command.name, func(t *testing.T) {
			t.Parallel()

			input, ok := inputs[command.name]
			require.True(t, ok, "missing test input for %s", command.name)

			response, err := buildCodeIntelResponse(t.Context(), root, input, command.name)
			require.NoError(t, err)
			assert.Equal(t, codeIntelSchemaVersion, response.Schema)
			assert.Equal(t, command.name, response.Command)
			assert.Equal(t, descriptors[command.name].TextKind, response.TextKind)

			var textOut bytes.Buffer
			require.NoError(t, writeCodeIntelResponse(&textOut, response, outputFormatText))
			assert.NotEmpty(t, textOut.String())

			var jsonOut bytes.Buffer
			require.NoError(t, writeCodeIntelResponse(&jsonOut, response, outputFormatJSON))

			var decoded codeIntelResponse
			require.NoError(t, json.Unmarshal(jsonOut.Bytes(), &decoded))
			assert.Equal(t, command.name, decoded.Command)

			if command.name != codeIntelCyclesCommandName {
				assert.False(t, decoded.Empty, "fixture should exercise a non-empty response for %s", command.name)
			}

			assertCodeIntelJSONIncludesDocumentedPayload(t, command.name, jsonOut.Bytes(), decoded.Empty)
		})
	}
}

func TestCodeIntelDescriptorsSelectExpectedCommands(t *testing.T) {
	t.Parallel()

	inputs := codeIntelTestInputsByCommand()

	for _, descriptor := range codeIntelCommandDescriptors() {
		t.Run(descriptor.Name, func(t *testing.T) {
			t.Parallel()

			input, ok := inputs[descriptor.Name]
			require.True(t, ok, "missing selector test input for %s", descriptor.Name)

			matches := matchingCodeIntelCommands(input)
			require.Len(t, matches, 1)
			assert.Equal(t, descriptor.Name, matches[0].name)

			selected, err := selectCodeIntelCommand(input)
			require.NoError(t, err)
			require.NotNil(t, selected)
			assert.Equal(t, descriptor.Name, selected.name)
		})
	}
}

func codeIntelTestInputsByCommand() map[string]codeIntelCommandInput {
	return map[string]codeIntelCommandInput{
		codeIntelQueryCommandName:                 {ModelQuery: "definitions:Run"},
		"code-symbol-name":                        {SymbolName: "Run"},
		"code-symbol-file-summary":                {SymbolFileSummary: "Run"},
		"code-symbol-package-summary":             {SymbolPackageSummary: "Run"},
		"code-symbol-prefix":                      {SymbolPrefix: "Ru"},
		"code-symbol-prefix-file-summary":         {SymbolPrefixFileSummary: "Ru"},
		"code-symbol-prefix-package-summary":      {SymbolPrefixPackageSummary: "Ru"},
		"code-symbol-kind":                        {SymbolKind: "func"},
		"code-symbol-kind-file-summary":           {SymbolKindFileSummary: "func"},
		"code-symbol-kind-package-summary":        {SymbolKindPackageSummary: "func"},
		"list-code-symbol-summary":                {ListSymbolSummary: true},
		"list-code-symbol-file-summary":           {ListSymbolFileSummary: true},
		"list-code-imports":                       {ListImports: true},
		"list-code-import-summary":                {ListImportSummary: true},
		"list-code-import-file-summary":           {ListImportFileSummary: true},
		"code-import-path":                        {ImportPath: "context"},
		"code-import-path-summary":                {ImportPathSummary: "context"},
		"code-import-path-file-summary":           {ImportPathFileSummary: "context"},
		"code-import-path-package-summary":        {ImportPathPackageSummary: "context"},
		"code-import-prefix":                      {ImportPrefix: "con"},
		"code-import-prefix-summary":              {ImportPrefixSummary: "con"},
		"code-import-prefix-file-summary":         {ImportPrefixFileSummary: "con"},
		"code-import-prefix-package-summary":      {ImportPrefixPackageSummary: "con"},
		"list-code-packages":                      {ListPackages: true},
		"code-package-name":                       {PackageName: "main"},
		"list-code-package-import-summary":        {ListPackageImportSummary: true},
		"code-package-imports":                    {PackageImports: "main"},
		"code-package-import-path":                {PackageImportPath: "main:context"},
		"code-package-import-files":               {PackageImportFiles: "main:context"},
		"code-package-import-path-file-summary":   {PackageImportPathFileSummary: "main:context"},
		"code-package-import-prefix":              {PackageImportPrefix: "main:con"},
		"code-package-import-prefix-files":        {PackageImportPrefixFiles: "main:con"},
		"code-package-import-prefix-file-summary": {PackageImportPrefixFileSummary: "main:con"},
		"code-package-import-file-summary":        {PackageImportFileSummary: "main"},
		"code-package-symbols":                    {PackageSymbols: "main"},
		"code-package-symbol-file-summary":        {PackageSymbolFileSummary: "main"},
		"code-package-symbol-name":                {PackageSymbolName: "main:Run"},
		"code-package-symbol-name-file-summary":   {PackageSymbolNameFileSummary: "main:Run"},
		"code-package-symbol-list":                {PackageSymbolList: "main"},
		"code-package-symbol-kind":                {PackageSymbolKind: "main:func"},
		"code-package-symbol-kind-file-summary":   {PackageSymbolKindFileSummary: "main:func"},
		"code-package-symbol-prefix":              {PackageSymbolPrefix: "main:Ru"},
		"code-package-symbol-prefix-file-summary": {PackageSymbolPrefixFileSummary: "main:Ru"},
		"code-file-path":                          {FilePath: "runner.go"},
		"code-file-imports":                       {FileImports: "runner.go"},
		"code-file-symbols":                       {FileSymbols: "runner.go"},
		"code-file-symbol-summary":                {FileSymbolSummary: "runner.go"},
		"code-file-symbol-name":                   {FileSymbolName: "runner.go:Run"},
		"code-file-symbol-kind":                   {FileSymbolKind: "runner.go:func"},
		"code-file-symbol-prefix":                 {FileSymbolPrefix: "runner.go:Ru"},
		"code-file-import-prefix":                 {FileImportPrefix: "runner.go:con"},
		"code-file-import-path":                   {FileImportPath: "runner.go:context"},
		"list-code-layers":                        {ListLayers: true},
		codeIntelCyclesCommandName:                {ListCycles: true},
		"code-summary":                            {Summary: true},
		"list-code-files":                         {ListFiles: true},
		"code-impact-target":                      {ImpactTarget: "context"},
		"code-reach-target":                       {ReachTarget: "runner.go"},
		"code-deps-target":                        {DepsTarget: "runner.go"},
		"code-rdeps-target":                       {RDepsTarget: "context"},
	}
}

func TestCodeIntelOutputFormat_JSONFlagAndOutputFormat(t *testing.T) {
	t.Parallel()

	format, err := codeIntelOutputFormat(codeIntelCommandInput{JSON: true, OutputFormat: outputFormatText})
	require.NoError(t, err)

	if format != outputFormatJSON {
		t.Fatalf("expected JSON format from --json, got %q", format)
	}

	format, err = codeIntelOutputFormat(codeIntelCommandInput{OutputFormat: outputFormatJSON})
	require.NoError(t, err)

	if format != outputFormatJSON {
		t.Fatalf("expected JSON format from --output json, got %q", format)
	}

	_, err = codeIntelOutputFormat(codeIntelCommandInput{OutputFormat: "xml"})
	require.EqualError(t, err, `unsupported output format "xml" (supported: text, json)`)

	_, err = codeIntelOutputFormat(codeIntelCommandInput{JSON: true, OutputFormat: "xml"})
	require.EqualError(t, err, `unsupported output format "xml" (supported: text, json)`)
}

func TestRunCodeIntelCommandRejectsInvalidOutputBeforeIndexing(t *testing.T) {
	t.Parallel()

	var out bytes.Buffer

	err := runCodeIntelCommandWithWriter(t.Context(), &out, filepath.Join(t.TempDir(), "missing"), codeIntelCommandInput{
		SymbolName:   "Run",
		OutputFormat: "xml",
	})

	require.EqualError(t, err, `unsupported output format "xml" (supported: text, json)`)
	assert.Empty(t, out.String())
}

func TestBuildCodeIntelResponseRejectsUnknownCommandBeforeIndexing(t *testing.T) {
	t.Parallel()

	_, err := buildCodeIntelResponse(t.Context(), filepath.Join(t.TempDir(), "missing"), codeIntelCommandInput{}, "missing-command")

	require.EqualError(t, err, `unsupported code-intel command "missing-command"`)
}

func TestRunCodeIntelCommandWritesSelectedSchemaOutput(t *testing.T) {
	t.Parallel()

	root := writeCodeIntelSchemaFixture(t)

	var text bytes.Buffer
	require.NoError(t, runCodeIntelCommandWithWriter(t.Context(), &text, root, codeIntelCommandInput{SymbolName: "Run"}))
	assert.Equal(t, "Run\tkind=func\tpath=runner.go\tline=3\n", text.String())

	var jsonOut bytes.Buffer
	require.NoError(t, runCodeIntelCommandWithWriter(t.Context(), &jsonOut, root, codeIntelCommandInput{SymbolName: "Run", OutputFormat: outputFormatJSON}))
	assert.JSONEq(t, `{
		"schema": "atteler.code_intel.v1",
		"command": "code-symbol-name",
		"query": {"symbol": "Run"},
		"empty": false,
		"symbols": [{"name": "Run", "kind": "func", "path": "runner.go", "line": 3}]
	}`, jsonOut.String())
}

func TestRunCodeIntelCommandPermissionPolicyDeniesWorkspaceRead(t *testing.T) {
	t.Parallel()

	root := writeCodeIntelSchemaFixture(t)

	policy := permission.DefaultPolicy()
	policy.SetMode(permission.OperationRead, permission.ModeDeny)

	ctx := permission.ContextWithPolicy(t.Context(), &policy)

	var out bytes.Buffer

	err := runCodeIntelCommandWithWriterContext(ctx, &out, root, codeIntelCommandInput{SymbolName: "Run"})

	require.Error(t, err)
	require.True(t, permission.ErrDenied(err))
	assert.Contains(t, err.Error(), "permission.read.deny")
	assert.Empty(t, out.String())
}

func TestRunCodeIntelCommandGroupedJSONRendersSchemaOutput(t *testing.T) {
	t.Parallel()

	root := writeCodeIntelSchemaFixture(t)
	opts := parseGroupedOptionsForRouteTest(t, []string{
		codeIntelDomainName,
		"symbol",
		"Run",
		"--json",
	})

	var out bytes.Buffer
	require.NoError(t, runCodeIntelCommandWithWriter(t.Context(), &out, root, codeIntelCommandInputFromOptions(opts)))

	assert.JSONEq(t, `{
		"schema": "atteler.code_intel.v1",
		"command": "code-symbol-name",
		"query": {"symbol": "Run"},
		"empty": false,
		"symbols": [{"name": "Run", "kind": "func", "path": "runner.go", "line": 3}]
	}`, out.String())
}

func TestSelectCodeIntelCommandRejectsAmbiguousSelectors(t *testing.T) {
	t.Parallel()

	_, err := selectCodeIntelCommand(codeIntelCommandInput{
		Summary:      true,
		ListFiles:    true,
		OutputFormat: outputFormatJSON,
		JSON:         true,
	})

	require.EqualError(t, err, "ambiguous CLI command: flags match multiple code-intel commands (code-summary, list-code-files); choose one command or remove conflicting flags")
}

func TestCodeIntelResponse_InvalidPairSpecErrorsNameFields(t *testing.T) {
	t.Parallel()

	root := writeCodeIntelSchemaFixture(t)
	tests := []struct {
		name  string
		want  string
		input codeIntelCommandInput
	}{
		{
			name:  "code-package-import-path",
			input: codeIntelCommandInput{PackageImportPath: "main:"},
			want:  "code package import path: package and import are required",
		},
		{
			name:  "code-package-symbol-kind",
			input: codeIntelCommandInput{PackageSymbolKind: "main:"},
			want:  "code package symbol kind: package and kind are required",
		},
		{
			name:  "code-file-import-prefix",
			input: codeIntelCommandInput{FileImportPrefix: "runner.go:"},
			want:  "code file import prefix: path and prefix are required",
		},
		{
			name:  "code-file-symbol-name",
			input: codeIntelCommandInput{FileSymbolName: "runner.go:"},
			want:  "code file symbol: path and symbol are required",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			_, err := buildCodeIntelResponse(t.Context(), root, test.input, test.name)
			require.Error(t, err)
			assert.EqualError(t, err, test.want)
		})
	}
}

func TestCodeIntelCommandDescriptorsDriveRegistryDocsAndFlags(t *testing.T) {
	t.Parallel()

	descriptors := codeIntelCommandDescriptors()
	require.NotEmpty(t, descriptors)

	descriptorNames := make(map[string]bool, len(descriptors))
	domainCommands := make(map[string]bool, len(descriptors))
	legacyFlags := make(map[string]bool, len(descriptors))

	for _, descriptor := range descriptors {
		assert.NotEmpty(t, descriptor.Name)
		assert.NotEmpty(t, descriptor.DomainCommand)
		assert.NotEmpty(t, descriptor.LegacyFlag)
		assert.NotEmpty(t, descriptor.Summary)
		assert.NotNil(t, descriptor.Match)
		assert.True(t, strings.HasPrefix(descriptor.LegacyFlag, "--"))
		assert.False(t, descriptorNames[descriptor.Name], "duplicate descriptor name %s", descriptor.Name)
		assert.False(t, domainCommands[descriptor.DomainCommand], "duplicate code-intel domain command %s", descriptor.DomainCommand)
		assert.False(t, legacyFlags[descriptor.LegacyFlag], "duplicate code-intel legacy flag %s", descriptor.LegacyFlag)

		descriptorNames[descriptor.Name] = true
		domainCommands[descriptor.DomainCommand] = true
		legacyFlags[descriptor.LegacyFlag] = true
	}

	for _, alias := range codeIntelLSPDomainCommandAliases() {
		assert.NotEmpty(t, alias.Name)
		assert.NotEmpty(t, alias.Summary)
		assert.NotEmpty(t, alias.TextOutput)
		assert.Equal(t, codeIntelSchemaVersion, alias.JSONSchema)
		assert.NotEmpty(t, alias.JSONFields)
		require.NotEmpty(t, alias.Legacy)

		assert.False(t, domainCommands[alias.Name], "duplicate LSP code-intel domain command %s", alias.Name)
		assert.False(t, legacyFlags[alias.Legacy[0]], "duplicate LSP code-intel legacy flag %s", alias.Legacy[0])

		domainCommands[alias.Name] = true
		legacyFlags[alias.Legacy[0]] = true
	}

	focusedNames := focusedCodeIntelCommandDescriptorNames()
	require.NotEmpty(t, focusedNames)

	for _, name := range focusedNames {
		assert.True(t, descriptorNames[name], "focused code-intel help references unknown descriptor %s", name)
	}

	commandsByName := make(map[string]codeIntelCommand, len(descriptors))
	for _, command := range codeIntelCommands() {
		commandsByName[command.name] = command
	}

	flags := stringSet(codeIntelInputFlags())

	for _, descriptor := range descriptors {
		t.Run(descriptor.Name, func(t *testing.T) {
			t.Parallel()

			assert.Contains(t, commandsByName, descriptor.Name)
			assert.True(t, flags[descriptor.LegacyFlag], "missing input flag %s", descriptor.LegacyFlag)
			assert.NotEmpty(t, descriptor.TextKind, "missing descriptor text kind for %s", descriptor.Name)

			textKind := codeIntelTextKindForCommand(descriptor.Name)
			assert.Equal(t, descriptor.TextKind, textKind)
			assert.NotEmpty(t, textKind, "missing text kind for %s", descriptor.Name)
			assert.NotEmpty(t, codeIntelTextOutputForKind(textKind), "missing text output docs for %s", descriptor.Name)
			assert.Contains(t, codeIntelJSONFieldsForKind(textKind), "schema")
		})
	}

	descriptorsByLegacyFlag := make(map[string]codeIntelCommandDescriptor, len(descriptors))
	for _, descriptor := range descriptors {
		descriptorsByLegacyFlag[descriptor.LegacyFlag] = descriptor
	}

	for _, domainCommand := range codeIntelDomainCommandAliases() {
		t.Run("docs/"+domainCommand.Name, func(t *testing.T) {
			t.Parallel()

			assert.Equal(t, codeIntelSchemaVersion, domainCommand.JSONSchema)

			if len(domainCommand.Legacy) == 0 || domainCommand.Legacy[0] == "--lsp-symbols" ||
				domainCommand.Legacy[0] == "--lsp-workspace-symbols" {
				assert.Equal(t, codeIntelTextOutputForKind(codeIntelTextLSPSymbols), domainCommand.TextOutput)
				assert.Equal(t, codeIntelJSONFieldsForKind(codeIntelTextLSPSymbols), domainCommand.JSONFields)

				return
			}

			descriptor, ok := descriptorsByLegacyFlag[domainCommand.Legacy[0]]
			require.True(t, ok, "missing descriptor for documented grouped command %s", domainCommand.Legacy[0])
			assert.Equal(t, descriptor.DomainCommand, domainCommand.Name)
			assert.Equal(t, descriptor.Args, domainCommand.Args)
			assert.Equal(t, descriptor.Summary, domainCommand.Summary)
			assert.Equal(t, codeIntelTextOutputForKind(descriptor.TextKind), domainCommand.TextOutput)
			assert.Equal(t, codeIntelJSONFieldsForKind(descriptor.TextKind), domainCommand.JSONFields)
		})
	}

	expectedDomainCommands := len(descriptors) + 2 // plus lsp-symbols and lsp-workspace.
	assert.Len(t, codeIntelDomainCommandAliases(), expectedDomainCommands)

	focusedCommands := focusedCodeIntelDomainCommandAliases()
	assert.Len(t, focusedCommands, len(focusedNames)+2) // plus lsp-symbols and lsp-workspace.

	for _, domainCommand := range focusedCommands {
		if len(domainCommand.Legacy) > 0 && (domainCommand.Legacy[0] == "--lsp-symbols" ||
			domainCommand.Legacy[0] == "--lsp-workspace-symbols") {
			continue
		}

		assert.True(t, domainCommands[domainCommand.Name], "focused code-intel help exposes unknown domain command %s", domainCommand.Name)
	}
}

func TestCodeIntelDomainExamplesAreDescriptorDerived(t *testing.T) {
	t.Parallel()

	descriptors := codeIntelCommandDescriptorsByName()
	assert.Equal(t, []string{
		codeIntelCommandExample(descriptors[codeIntelSummaryCommandName]),
		codeIntelCommandExample(descriptors[codeIntelSummaryCommandName], "--json"),
		codeIntelCommandExample(descriptors[codeIntelQueryCommandName], "definitions:Run"),
		codeIntelCommandExample(descriptors["code-symbol-name"], "NewRegistry"),
		codeIntelCommandExample(descriptors["code-import-prefix"], "github.com/tommoulard/atteler/pkg/"),
	}, codeIntelDomainExamples())

	surface := buildCommandSurface(commandRegistry)
	commands := commandSurfaceCommandsByName(surface.Commands)
	assert.Equal(t, codeIntelDomainExamples(), commands["code-intel"].Examples)
}

func TestCodeIntelDescriptorExamplesUseDomainSpecificPrefixValues(t *testing.T) {
	t.Parallel()

	descriptors := codeIntelCommandDescriptorsByName()

	assert.Equal(t,
		[]string{"atteler code-intel package-import-prefix main:con"},
		descriptors["code-package-import-prefix"].examples(),
	)
	assert.Equal(t,
		[]string{"atteler code-intel package-symbol-prefix main:Ru"},
		descriptors["code-package-symbol-prefix"].examples(),
	)
	assert.Equal(t,
		[]string{"atteler code-intel rdeps context"},
		descriptors["code-rdeps-target"].examples(),
	)
}

func TestCodeIntelCommandSurfaceIncludesGeneratedOutputDocs(t *testing.T) {
	t.Parallel()

	surface := buildCommandSurface(commandRegistry)
	summary := requireDomainCommand(t, surface, "code-intel", "summary")
	lspWorkspace := requireDomainCommand(t, surface, "code-intel", "lsp-workspace")
	symbolNameFileSummary := requireDomainRoutingCommand(t, surface, "code-intel", "symbol-name-file-summary")

	assert.Equal(t, codeIntelTextOutputForKind(codeIntelTextSummary), summary.TextOutput)
	assert.Equal(t, codeIntelSchemaVersion, summary.JSONSchema)
	assert.Contains(t, summary.JSONFields, "summary")
	assert.Equal(t, codeIntelTextOutputForKind(codeIntelTextLSPSymbols), lspWorkspace.TextOutput)
	assert.Equal(t, codeIntelSchemaVersion, lspWorkspace.JSONSchema)
	assert.Contains(t, lspWorkspace.JSONFields, "lsp_symbols")
	assert.Contains(t, lspWorkspace.OutputModes, commandOutputJSON)
	assert.Equal(t, codeIntelTextOutputForKind(codeIntelTextSymbolFileSummary), symbolNameFileSummary.TextOutput)
	assert.Equal(t, codeIntelSchemaVersion, symbolNameFileSummary.JSONSchema)
	assert.Contains(t, symbolNameFileSummary.JSONFields, "files")
	assert.Contains(t, symbolNameFileSummary.OutputModes, commandOutputJSON)

	docs := renderCommandSurfaceMarkdown(surface)
	assert.Contains(t, docs, "Text output")
	assert.Contains(t, docs, codeIntelTextOutputForKind(codeIntelTextSummary))
	assert.Contains(t, docs, "`symbol-name-file-summary <name>`: list files with symbol counts for one exact name")
	assert.Contains(t, docs, codeIntelTextOutputForKind(codeIntelTextSymbolFileSummary))
	assert.Contains(t, docs, codeIntelTextOutputForKind(codeIntelTextLSPSymbols))
	assert.Contains(t, docs, "JSON schema")
	assert.Contains(t, docs, codeIntelSchemaVersion)
	assert.NotContains(t, docs, "JSON schema: ``")
	assert.Contains(t, docs, "JSON fields")
}

func TestCodeIntelCommandSurfaceOutputDocsCoverEveryDomainCommand(t *testing.T) {
	t.Parallel()

	surface := buildCommandSurface(commandRegistry)
	docs := renderCommandSurfaceMarkdown(surface)

	var codeIntelDomain commandSurfaceDomain

	foundDomain := false

	for i := range surface.Domains {
		if surface.Domains[i].Name != codeIntelDomainName {
			continue
		}

		codeIntelDomain = surface.Domains[i]
		foundDomain = true

		break
	}

	require.True(t, foundDomain, "missing code-intel command-surface domain")

	commands := append([]commandSurfaceDomainCommand(nil), codeIntelDomain.Commands...)
	commands = append(commands, codeIntelDomain.RoutingCommands...)
	require.NotEmpty(t, commands)

	for i := range commands {
		command := commands[i]
		t.Run(command.Name, func(t *testing.T) {
			t.Parallel()

			assert.NotEmpty(t, command.DispatchCommands)
			assert.Contains(t, command.OutputModes, commandOutputText)
			assert.Contains(t, command.OutputModes, commandOutputJSON)
			assert.NotEmpty(t, command.TextOutput)
			assert.Equal(t, codeIntelSchemaVersion, command.JSONSchema)
			assert.Contains(t, command.JSONFields, "schema")
			assert.Contains(t, command.JSONFields, "command")
			assert.Contains(t, command.JSONFields, "empty")
			assert.NotEmpty(t, command.Examples)

			renderedCommand := "`" + command.Name
			if command.Args != "" {
				renderedCommand += " " + command.Args
			}

			renderedCommand += "`"

			assert.Contains(t, docs, renderedCommand)
			assert.Contains(t, docs, command.TextOutput)
			assert.Contains(t, docs, command.JSONSchema)

			for _, field := range command.JSONFields {
				assert.Contains(t, docs, field)
			}

			for _, example := range command.Examples {
				assert.Contains(t, docs, example)
			}
		})
	}
}

func TestCodeIntelCommandSurfaceDomainExamplesStayParseable(t *testing.T) {
	t.Parallel()

	surface := buildCommandSurface(commandRegistry)

	var codeIntelDomain commandSurfaceDomain

	foundDomain := false

	for i := range surface.Domains {
		if surface.Domains[i].Name != codeIntelDomainName {
			continue
		}

		codeIntelDomain = surface.Domains[i]
		foundDomain = true

		break
	}

	require.True(t, foundDomain, "missing code-intel command-surface domain")

	commands := append([]commandSurfaceDomainCommand(nil), codeIntelDomain.Commands...)
	commands = append(commands, codeIntelDomain.RoutingCommands...)
	require.NotEmpty(t, commands)

	for i := range commands {
		command := commands[i]
		for _, example := range command.Examples {
			t.Run(command.Name+"/"+example, func(t *testing.T) {
				t.Parallel()

				assert.NotContains(t, example, "<", "domain command examples should use concrete values")
				assert.NotContains(t, example, ">", "domain command examples should use concrete values")

				args := splitCommandLineForTest(t, example)
				require.NotEmpty(t, args)
				require.Equal(t, "atteler", args[0])

				fs := newRegisteredFlagSetForTest(t)
				plan := translateCLIArgsWithFlagSet(args[1:], fs)
				require.NoError(t, plan.Err)
				require.False(t, plan.Help)
				require.NoError(t, fs.Parse(plan.Args), "domain command example should parse after translation: %s -> %#v", example, plan.Args)
			})
		}
	}
}

func TestCodeIntelCommandSurfaceExamplesMatchQueryDocs(t *testing.T) {
	t.Parallel()

	surface := buildCommandSurface(commandRegistry)

	queryDocs := append(codeIntelQueryDocs(), codeIntelLSPQueryDocs()...)

	examplesByDomainCommand := make(map[string][]string, len(queryDocs))

	for _, query := range queryDocs {
		examplesByDomainCommand[query.DomainCommand] = query.Examples
	}

	var codeIntelDomain commandSurfaceDomain

	foundDomain := false

	for i := range surface.Domains {
		if surface.Domains[i].Name != codeIntelDomainName {
			continue
		}

		codeIntelDomain = surface.Domains[i]
		foundDomain = true

		break
	}

	require.True(t, foundDomain, "missing code-intel command-surface domain")

	commands := append([]commandSurfaceDomainCommand(nil), codeIntelDomain.Commands...)
	commands = append(commands, codeIntelDomain.RoutingCommands...)
	require.NotEmpty(t, commands)

	for _, command := range commands {
		assert.Equal(t, examplesByDomainCommand[command.Name], command.Examples, "examples for %s should come from the query descriptor docs", command.Name)
	}
}

func TestCodeIntelLSPDispatchContractIncludesQueryDocs(t *testing.T) {
	t.Parallel()

	surface := buildCommandSurface(commandRegistry)
	commands := commandSurfaceCommandsByName(surface.Commands)
	lspSymbols := commands["lsp-symbols"]

	require.Len(t, lspSymbols.CodeIntelQueries, 2)
	assert.Equal(t, codeIntelLSPQueryDocs(), lspSymbols.CodeIntelQueries)
	assertCodeIntelQueryDoc(t, lspSymbols.CodeIntelQueries, "lsp-symbols", "lsp-symbols", "lsp_symbols")
	assertCodeIntelQueryDoc(t, lspSymbols.CodeIntelQueries, "lsp-workspace", "lsp-workspace", "lsp_symbols")

	docs := renderCommandSurfaceMarkdown(surface)
	assert.Contains(t, docs, "`lsp-symbols` / `--lsp-symbols`")
	assert.Contains(t, docs, "`lsp-workspace <query>` / `--lsp-workspace-symbols`")
	assert.Contains(t, docs, codeIntelSchemaVersion)
	assert.Contains(t, docs, "lsp_symbols")
}

func TestCodeIntelDispatchContractsSeparateGoIndexAndLSPProcessEffects(t *testing.T) {
	t.Parallel()

	surface := buildCommandSurface(commandRegistry)
	commands := commandSurfaceCommandsByName(surface.Commands)

	goIndex := commands[codeIntelDomainName]
	require.NotEmpty(t, goIndex)
	assert.ElementsMatch(t, []string{commandEffectFilesystemRead, commandEffectUserOutput}, goIndex.SideEffects)
	assert.NotContains(t, goIndex.InputFlags, "--lsp-symbols")
	assert.NotContains(t, goIndex.InputFlags, "--lsp-workspace-symbols")
	assert.Contains(t, goIndex.InputFlags, "--code-symbol")

	lspSymbols := commands[codeIntelLSPSymbolsName]
	require.NotEmpty(t, lspSymbols)
	assert.Contains(t, lspSymbols.SideEffects, commandEffectProcessExecute)
	assert.Contains(t, lspSymbols.InputFlags, "--lsp-symbols")

	summaryCommand := requireDomainCommand(t, surface, codeIntelDomainName, "summary")
	assert.NotContains(t, summaryCommand.SideEffects, commandEffectProcessExecute)

	lspCommand := requireDomainCommand(t, surface, codeIntelDomainName, codeIntelLSPSymbolsName)
	assert.Contains(t, lspCommand.SideEffects, commandEffectProcessExecute)
}

func TestCodeIntelCommandSurfaceJSONIncludesSchemaDocs(t *testing.T) {
	t.Parallel()

	var out strings.Builder
	require.NoError(t, printCommandSurfaceJSON(&out))

	var surface commandSurface
	require.NoError(t, json.Unmarshal([]byte(out.String()), &surface))

	summary := requireDomainCommand(t, surface, "code-intel", "summary")
	symbolNameFileSummary := requireDomainRoutingCommand(t, surface, "code-intel", "symbol-name-file-summary")
	assert.Equal(t, codeIntelSchemaVersion, summary.JSONSchema)
	assert.Contains(t, summary.JSONFields, "summary")
	assert.Equal(t, codeIntelSchemaVersion, symbolNameFileSummary.JSONSchema)
	assert.Contains(t, symbolNameFileSummary.JSONFields, "files")

	commands := commandSurfaceCommandsByName(surface.Commands)
	assertCodeIntelQueryDoc(t, commands["code-intel"].CodeIntelQueries, "code-summary", "summary", "summary")
	assertCodeIntelQueryDoc(t, commands["lsp-symbols"].CodeIntelQueries, "lsp-symbols", "lsp-symbols", "lsp_symbols")
	assertCodeIntelQueryDoc(t, commands["lsp-symbols"].CodeIntelQueries, "lsp-workspace", "lsp-workspace", "lsp_symbols")
}

func TestCodeIntelJSONFieldDocsMatchResponseSchema(t *testing.T) {
	t.Parallel()

	responseFields := codeIntelResponseJSONFieldsForTest()
	commonFields := stringSet([]string{"schema", "command", "query", "empty", "message", "pagination"})

	textKinds := make(map[codeIntelTextKind]bool)
	for _, descriptor := range codeIntelCommandDescriptors() {
		textKinds[descriptor.TextKind] = true
	}

	textKinds[codeIntelTextLSPSymbols] = true

	for textKind := range textKinds {
		t.Run(string(textKind), func(t *testing.T) {
			t.Parallel()

			fields := codeIntelJSONFieldsForKind(textKind)
			require.NotEmpty(t, fields)

			payloadField, _, knownPayload := codeIntelTextPayloadPresent(codeIntelResponse{TextKind: textKind})
			require.True(t, knownPayload, "missing text payload validation for %s", textKind)
			assert.Contains(t, fields, payloadField, "JSON docs for %s should include renderer payload field", textKind)

			payloadFields := 0

			for _, field := range fields {
				assert.True(t, responseFields[field], "documented JSON field %q for %s is not in codeIntelResponse", field, textKind)

				if !commonFields[field] {
					payloadFields++
				}
			}

			assert.Positive(t, payloadFields, "%s should document at least one payload field", textKind)
		})
	}
}

func TestCodeIntelFormattingIsCentralized(t *testing.T) {
	t.Parallel()

	files, err := filepath.Glob("codeintel*.go")
	require.NoError(t, err)
	require.NotEmpty(t, files)

	disallowed := []string{"fmt.Print", "fmt.Fprint", "fmt.Sprintf", "os.Stdout", "func formatCode"}

	for _, path := range files {
		if strings.HasSuffix(path, "_test.go") || filepath.Base(path) == "codeintel_response_render.go" {
			continue
		}

		data, err := os.ReadFile(path)
		require.NoError(t, err)

		content := string(data)
		for _, marker := range disallowed {
			assert.NotContains(t, content, marker, "%s should render only through codeintel_response_render.go", path)
		}
	}
}

func TestCodeIntelSortingIsCentralized(t *testing.T) {
	t.Parallel()

	files, err := filepath.Glob("codeintel*.go")
	require.NoError(t, err)
	require.NotEmpty(t, files)

	for _, path := range files {
		base := filepath.Base(path)
		if strings.HasSuffix(path, "_test.go") || base == "codeintel_collection_helpers.go" {
			continue
		}

		data, err := os.ReadFile(path)
		require.NoError(t, err)

		content := string(data)
		assert.NotContains(t, content, "sort.Slice", "%s should sort through codeintel_collection_helpers.go", path)
		assert.NotContains(t, content, "sort.Strings", "%s should sort through codeintel_collection_helpers.go", path)
		assert.NotContains(t, content, `"sort"`, "%s should not import sort directly", path)
	}
}

func TestCodeIntelFilteringIsCentralized(t *testing.T) {
	t.Parallel()

	files, err := filepath.Glob("codeintel*.go")
	require.NoError(t, err)
	require.NotEmpty(t, files)

	for _, path := range files {
		base := filepath.Base(path)
		if strings.HasSuffix(path, "_test.go") || base == "codeintel_collection_helpers.go" {
			continue
		}

		data, err := os.ReadFile(path)
		require.NoError(t, err)

		content := string(data)
		assert.NotContains(t, content, "filtered := make", "%s should filter through codeintel_collection_helpers.go", path)
		assert.NotContains(t, content, "matches := make", "%s should filter through codeintel_collection_helpers.go", path)
	}
}

func TestCodeIntelIndexingIsCentralized(t *testing.T) {
	t.Parallel()

	files, err := filepath.Glob("codeintel*.go")
	require.NoError(t, err)
	require.NotEmpty(t, files)

	for _, path := range files {
		base := filepath.Base(path)
		if strings.HasSuffix(path, "_test.go") || base == "codeintel_response_builders.go" {
			continue
		}

		data, err := os.ReadFile(path)
		require.NoError(t, err)

		assert.NotContains(t, string(data), "codeintel.IndexDir", "%s should use the shared response-builder index path", path)
	}
}

func TestCodeIntelResponseTextKindIsDescriptorDriven(t *testing.T) {
	t.Parallel()

	data, err := os.ReadFile("codeintel_response_builders.go")
	require.NoError(t, err)

	content := string(data)
	assert.Contains(t, content, "textKind := codeIntelTextKindForCommand(commandName)")
	assert.Contains(t, content, "response.TextKind = textKind")
	assert.Equal(t, 1, strings.Count(content, "response.TextKind ="), "builders should derive text rendering from command descriptors once")
	assert.NotContains(t, content, "codeIntelTextKindsByCommand")
}

func TestCodeIntelResponseBuildersOnlyHandleDescribedCommands(t *testing.T) {
	t.Parallel()

	builderFiles, err := filepath.Glob("codeintel*response_builders.go")
	require.NoError(t, err)
	require.NotEmpty(t, builderFiles)

	descriptorNames := make(map[string]bool)
	for _, descriptor := range codeIntelCommandDescriptors() {
		descriptorNames[descriptor.Name] = true
	}

	builderCases := make(map[string]int)
	caseNames := map[string]string{
		"codeIntelQueryCommandName":  codeIntelQueryCommandName,
		"codeIntelCyclesCommandName": codeIntelCyclesCommandName,
	}
	casePattern := regexp.MustCompile(`case (?:"([^"]+)"|(codeIntel[A-Za-z0-9_]*CommandName))`)
	ifPattern := regexp.MustCompile(`if commandName == (?:"([^"]+)"|(codeIntel[A-Za-z0-9_]*CommandName))`)

	for _, path := range builderFiles {
		data, err := os.ReadFile(path)
		require.NoError(t, err)

		for _, match := range append(casePattern.FindAllStringSubmatch(string(data), -1), ifPattern.FindAllStringSubmatch(string(data), -1)...) {
			require.Len(t, match, 3)

			commandName := match[1]
			if commandName == "" {
				var ok bool

				commandName, ok = caseNames[match[2]]
				require.True(t, ok, "response builder uses unknown command-name constant %s", match[2])
			}

			builderCases[commandName]++

			assert.True(t, descriptorNames[commandName], "response builder handles %s without a command descriptor", commandName)
		}
	}

	for descriptorName := range descriptorNames {
		assert.Equal(t, 1, builderCases[descriptorName], "descriptor %s should have exactly one response builder case", descriptorName)
	}
}

func TestCodeIntelDispatchContractIncludesEveryQueryDoc(t *testing.T) {
	t.Parallel()

	surface := buildCommandSurface(commandRegistry)
	commands := commandSurfaceCommandsByName(surface.Commands)
	codeIntel := commands["code-intel"]

	require.Len(t, codeIntel.CodeIntelQueries, len(codeIntelCommandDescriptors()))
	assert.Equal(t, codeIntelQueryDocs(), codeIntel.CodeIntelQueries)
	assertCodeIntelQueryDoc(t, codeIntel.CodeIntelQueries, "code-summary", "summary", "summary")
	assertCodeIntelQueryDoc(t, codeIntel.CodeIntelQueries, "code-symbol-name", "symbol", "symbols")

	docs := renderCommandSurfaceMarkdown(surface)
	assert.Contains(t, docs, "Code-intel queries")
	assert.Contains(t, docs, "`symbol <name>` / `--code-symbol`")

	queryDocs := codeIntelQueryDocs()
	for i := range queryDocs {
		query := &queryDocs[i]

		label := "`" + query.DomainCommand
		if query.Args != "" {
			label += " " + query.Args
		}

		label += "` / `" + query.LegacyFlag + "`"
		assert.Contains(t, docs, label, "missing rendered docs for %s", query.Name)
		assert.Contains(t, docs, query.TextOutput, "missing text output docs for %s", query.Name)
		assert.Contains(t, docs, query.JSONSchema, "missing JSON schema docs for %s", query.Name)

		for _, field := range query.JSONFields {
			assert.Contains(t, docs, field, "missing JSON field docs for %s", query.Name)
		}

		for _, example := range query.Examples {
			assert.Contains(t, docs, example, "missing example docs for %s", query.Name)
		}
	}
}

func TestCodeIntelQueryDocExamplesStayParseable(t *testing.T) {
	t.Parallel()

	queryDocs := append(codeIntelQueryDocs(), codeIntelLSPQueryDocs()...)
	for _, query := range queryDocs {
		for _, example := range query.Examples {
			t.Run(query.Name+"/"+example, func(t *testing.T) {
				t.Parallel()

				assert.NotContains(t, example, "<", "query doc examples should use concrete values")
				assert.NotContains(t, example, ">", "query doc examples should use concrete values")

				args := splitCommandLineForTest(t, example)
				require.NotEmpty(t, args)
				require.Equal(t, "atteler", args[0])

				fs := newRegisteredFlagSetForTest(t)
				plan := translateCLIArgsWithFlagSet(args[1:], fs)
				require.NoError(t, plan.Err)
				require.False(t, plan.Help)
				require.NoError(t, fs.Parse(plan.Args), "query doc example should parse after translation: %s -> %#v", example, plan.Args)
			})
		}
	}
}

func writeCodeIntelSchemaFixture(t *testing.T) string {
	t.Helper()

	root := t.TempDir()
	source := "package main\n\nfunc Run() {}\n"
	require.NoError(t, os.WriteFile(filepath.Join(root, "runner.go"), []byte(source), 0o600))

	return root
}

func writeCodeIntelCycleSchemaFixture(t *testing.T) string {
	t.Helper()

	root := t.TempDir()
	aSource := `package main

import _ "b.go"

func A() {}
`
	bSource := `package main

import _ "a.go"

func B() {}
`

	require.NoError(t, os.WriteFile(filepath.Join(root, "a.go"), []byte(aSource), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(root, "b.go"), []byte(bSource), 0o600))

	return root
}

func writeCodeIntelFullSchemaFixture(t *testing.T) string {
	t.Helper()

	root := t.TempDir()
	source := `package main

import "context"

type Runner struct{}

const Value = 1

func Run(ctx context.Context) {}
`
	require.NoError(t, os.WriteFile(filepath.Join(root, "runner.go"), []byte(source), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(root, "worker.py"), []byte("def helper():\n\treturn 'ok'\n"), 0o600))

	return root
}

func readCodeIntelGolden(t *testing.T, name string) string {
	t.Helper()

	data, err := os.ReadFile(filepath.Join("testdata", "codeintel", name))
	require.NoError(t, err)

	return string(data)
}

func assertCodeIntelQueryDoc(t *testing.T, queries []codeIntelQueryDoc, name, domainCommand, jsonField string) {
	t.Helper()

	for i := range queries {
		query := &queries[i]

		if query.Name != name {
			continue
		}

		assert.Equal(t, domainCommand, query.DomainCommand)
		assert.NotEmpty(t, query.Examples)
		assert.NotEmpty(t, query.TextOutput)
		assert.Equal(t, codeIntelSchemaVersion, query.JSONSchema)
		assert.Contains(t, query.JSONFields, jsonField)

		return
	}

	require.Failf(t, "missing code-intel query doc", "query %s not found", name)
}

func assertCodeIntelJSONIncludesDocumentedPayload(t *testing.T, commandName string, data []byte, empty bool) {
	t.Helper()

	if empty {
		return
	}

	var raw map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(data, &raw))

	for _, field := range codeIntelJSONFieldsForKind(codeIntelTextKindForCommand(commandName)) {
		switch field {
		case "schema", "command", "query", "empty", "message", "pagination":
			continue
		default:
			assert.Contains(t, raw, field, "non-empty JSON response for %s should include documented payload field %q", commandName, field)
		}
	}
}

func codeIntelResponseJSONFieldsForTest() map[string]bool {
	fields := make(map[string]bool)
	responseType := reflect.TypeFor[codeIntelResponse]()

	for field := range responseType.Fields() {
		jsonName := field.Tag.Get("json")
		if jsonName == "" || jsonName == "-" {
			continue
		}

		name, _, _ := strings.Cut(jsonName, ",")
		if name != "" {
			fields[name] = true
		}
	}

	return fields
}
