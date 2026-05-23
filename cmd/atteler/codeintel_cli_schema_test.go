package main

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tommoulard/atteler/pkg/lsp"
)

func TestCodeIntelQueryDocExamplesRunThroughCLIAsJSON(t *testing.T) {
	t.Parallel()

	root := writeCodeIntelFullSchemaFixture(t)

	for _, query := range codeIntelQueryDocs() {
		for _, example := range query.Examples {
			for _, variant := range codeIntelStructuredOutputVariants(example) {
				t.Run(query.Name+"/"+variant.name+"/"+example, func(t *testing.T) {
					t.Parallel()

					args := splitCommandLineForTest(t, variant.command)
					require.NotEmpty(t, args)
					require.Equal(t, "atteler", args[0])

					opts := parseGroupedOptionsForRouteTest(t, args[1:])

					var out bytes.Buffer
					require.NoError(t, runCodeIntelCommandWithWriter(&out, root, codeIntelCommandInputFromOptions(opts)))

					var decoded codeIntelResponse
					require.NoError(t, json.Unmarshal(out.Bytes(), &decoded))
					assert.Equal(t, codeIntelSchemaVersion, decoded.Schema)
					assert.Equal(t, query.Name, decoded.Command)

					if !codeIntelExampleMayBeEmpty(query.Name) {
						assert.False(t, decoded.Empty, "descriptor example should produce non-empty JSON output")
					}
				})
			}
		}
	}
}

func TestCodeIntelLegacyFlagsRunThroughCLIAsJSON(t *testing.T) {
	t.Parallel()

	root := writeCodeIntelFullSchemaFixture(t)

	for _, descriptor := range codeIntelCommandDescriptors() {
		for _, variant := range codeIntelLegacyFlagOutputVariants(descriptor) {
			t.Run(descriptor.Name+"/"+variant.name, func(t *testing.T) {
				t.Parallel()

				opts, fs := newCLIOptionsAndFlagSetForTest(t)
				require.NoError(t, fs.Parse(variant.args), "legacy args should parse: %#v", variant.args)
				applyPositionalOptions(opts, fs.Args())

				var out bytes.Buffer
				require.NoError(t, runCodeIntelCommandWithWriter(&out, root, codeIntelCommandInputFromOptions(*opts)))

				var decoded codeIntelResponse
				require.NoError(t, json.Unmarshal(out.Bytes(), &decoded))
				assert.Equal(t, codeIntelSchemaVersion, decoded.Schema)
				assert.Equal(t, descriptor.Name, decoded.Command)

				if !codeIntelExampleMayBeEmpty(descriptor.Name) {
					assert.False(t, decoded.Empty, "legacy flag should produce non-empty JSON output")
				}
			})
		}
	}
}

func TestCodeIntelLegacyFlagsRunThroughCLIAsText(t *testing.T) {
	t.Parallel()

	root := writeCodeIntelFullSchemaFixture(t)

	for _, descriptor := range codeIntelCommandDescriptors() {
		t.Run(descriptor.Name, func(t *testing.T) {
			t.Parallel()

			args := codeIntelLegacyFlagArgs(descriptor)
			opts, fs := newCLIOptionsAndFlagSetForTest(t)
			require.NoError(t, fs.Parse(args), "legacy args should parse: %#v", args)
			applyPositionalOptions(opts, fs.Args())

			var out bytes.Buffer
			require.NoError(t, runCodeIntelCommandWithWriter(&out, root, codeIntelCommandInputFromOptions(*opts)))
			assert.NotEmpty(t, out.String())

			if !codeIntelExampleMayBeEmpty(descriptor.Name) {
				assert.NotEqual(t, codeIntelEmptyMessage+"\n", out.String(), "legacy flag should produce non-empty text output")
			}
		})
	}
}

func TestCodeIntelPaginationFlagsReachSchemaOutputThroughCLI(t *testing.T) {
	t.Parallel()

	root := writeCodeIntelFullSchemaFixture(t)

	tests := []struct {
		name string
		args []string
	}{
		{
			name: "grouped command",
			args: []string{codeIntelDomainName, "symbol-summary", "--code-limit", "1", "--code-offset", "1", "--json"},
		},
		{
			name: "legacy flag",
			args: []string{"--code-symbol-summary", "--code-limit", "1", "--code-offset", "1", "--json"},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			opts := parseGroupedOptionsForRouteTest(t, test.args)

			var out bytes.Buffer
			require.NoError(t, runCodeIntelCommandWithWriter(&out, root, codeIntelCommandInputFromOptions(opts)))

			var decoded codeIntelResponse
			require.NoError(t, json.Unmarshal(out.Bytes(), &decoded))
			assert.Equal(t, "list-code-symbol-summary", decoded.Command)
			assert.False(t, decoded.Empty)
			require.NotNil(t, decoded.Pagination)
			assert.Equal(t, 1, *decoded.Pagination.Limit)
			assert.Equal(t, 1, decoded.Pagination.Offset)
			assert.Equal(t, 3, decoded.Pagination.Total)
			assert.Equal(t, 1, decoded.Pagination.Returned)
			assert.Len(t, decoded.Symbols, 1)
		})
	}
}

func TestCodeIntelQueryDocExamplesRunThroughCLIAsText(t *testing.T) {
	t.Parallel()

	root := writeCodeIntelFullSchemaFixture(t)

	for _, query := range codeIntelQueryDocs() {
		for _, example := range query.Examples {
			t.Run(query.Name+"/"+example, func(t *testing.T) {
				t.Parallel()

				args := splitCommandLineForTest(t, example)
				require.NotEmpty(t, args)
				require.Equal(t, "atteler", args[0])

				opts := parseGroupedOptionsForRouteTest(t, args[1:])

				var out bytes.Buffer
				require.NoError(t, runCodeIntelCommandWithWriter(&out, root, codeIntelCommandInputFromOptions(opts)))
				assert.NotEmpty(t, out.String())

				if !codeIntelExampleMayBeEmpty(query.Name) {
					assert.NotEqual(t, codeIntelEmptyMessage+"\n", out.String(), "descriptor example should produce non-empty text output")
				}
			})
		}
	}
}

func TestCodeIntelTextOutputDocsOnlyCoverRegisteredKinds(t *testing.T) {
	t.Parallel()

	registeredKinds := map[codeIntelTextKind]bool{
		codeIntelTextLSPSymbols: true,
	}
	for _, descriptor := range codeIntelCommandDescriptors() {
		registeredKinds[descriptor.TextKind] = true
	}

	for kind := range registeredKinds {
		assert.NotEmpty(t, codeIntelTextOutputForKind(kind), "registered code-intel text kind %s should have output docs", kind)
	}

	for kind := range codeIntelTextOutputsByKind {
		assert.True(t, registeredKinds[kind], "code-intel text output docs should not keep stale unregistered kind %s", kind)
	}
}

func TestCodeIntelLSPQueryDocExamplesRenderSchemaJSON(t *testing.T) {
	t.Parallel()

	for _, query := range codeIntelLSPQueryDocs() {
		for _, example := range query.Examples {
			for _, variant := range codeIntelStructuredOutputVariants(example) {
				t.Run(query.Name+"/"+variant.name+"/"+example, func(t *testing.T) {
					t.Parallel()

					args := splitCommandLineForTest(t, variant.command)
					require.NotEmpty(t, args)
					require.Equal(t, "atteler", args[0])

					opts := parseGroupedOptionsForRouteTest(t, args[1:])
					input := lspSymbolsCommandInputFromOptions(opts)
					format, err := structuredCommandOutputFormat(input.JSON, input.OutputFormat)
					require.NoError(t, err)
					requireCodeIntelJSONFormat(t, format)

					response := buildLSPCodeIntelResponse(input, []lsp.Symbol{{
						Name: "Run",
						Kind: 12,
					}})

					var out bytes.Buffer
					require.NoError(t, writeCodeIntelResponse(&out, response, format))

					var decoded codeIntelResponse
					require.NoError(t, json.Unmarshal(out.Bytes(), &decoded))
					assert.Equal(t, codeIntelSchemaVersion, decoded.Schema)
					assert.Equal(t, codeIntelLSPSymbolsName, decoded.Command)
					assert.False(t, decoded.Empty)
					assert.NotEmpty(t, decoded.LSPSymbols)
				})
			}
		}
	}
}

func TestCodeIntelLSPQueryDocExamplesRenderSchemaText(t *testing.T) {
	t.Parallel()

	for _, query := range codeIntelLSPQueryDocs() {
		for _, example := range query.Examples {
			t.Run(query.Name+"/"+example, func(t *testing.T) {
				t.Parallel()

				args := codeIntelTextExampleArgsForTest(t, example)
				opts := parseGroupedOptionsForRouteTest(t, args[1:])
				input := lspSymbolsCommandInputFromOptions(opts)
				format, err := structuredCommandOutputFormat(input.JSON, input.OutputFormat)
				require.NoError(t, err)
				assert.Equal(t, outputFormatText, format)

				response := buildLSPCodeIntelResponse(input, []lsp.Symbol{{
					Name: "Run",
					Kind: 12,
				}})

				var out bytes.Buffer
				require.NoError(t, writeCodeIntelResponse(&out, response, format))
				assert.Contains(t, out.String(), "Run\tkind=12")
			})
		}
	}
}

func TestCodeIntelLSPLegacyFlagsRenderSchemaOutputs(t *testing.T) {
	t.Parallel()

	assertLSPLegacySchemaOutputForTest(t, "document-json-flag",
		[]string{"--lsp-symbols", "--lsp-file", "main.go", "--json"},
		outputFormatJSON,
		map[string]string{"file": "main.go"},
	)
	assertLSPLegacySchemaOutputForTest(t, "workspace-output-json",
		[]string{"--lsp-workspace-symbols", "Handle", "--output", "json"},
		outputFormatJSON,
		map[string]string{"workspace_symbols": "Handle"},
	)
	assertLSPLegacySchemaOutputForTest(t, "document-text",
		[]string{"--lsp-symbols", "--lsp-file", "main.go"},
		outputFormatText,
		map[string]string{"file": "main.go"},
	)
}

func assertLSPLegacySchemaOutputForTest(t *testing.T, name string, args []string, wantFormat string, wantQuery map[string]string) {
	t.Helper()

	t.Run(name, func(t *testing.T) {
		t.Parallel()

		opts, fs := newCLIOptionsAndFlagSetForTest(t)
		require.NoError(t, fs.Parse(args), "legacy LSP args should parse: %#v", args)
		applyPositionalOptions(opts, fs.Args())

		input := lspSymbolsCommandInputFromOptions(*opts)
		format, err := structuredCommandOutputFormat(input.JSON, input.OutputFormat)
		require.NoError(t, err)
		assert.Equal(t, wantFormat, format)

		response := buildLSPCodeIntelResponse(input, []lsp.Symbol{{
			Name: "Run",
			Kind: 12,
		}})
		assert.Equal(t, wantQuery, response.Query)

		var out bytes.Buffer
		require.NoError(t, writeCodeIntelResponse(&out, response, format))

		if format == outputFormatJSON {
			var decoded codeIntelResponse
			require.NoError(t, json.Unmarshal(out.Bytes(), &decoded))
			assert.Equal(t, codeIntelSchemaVersion, decoded.Schema)
			assert.Equal(t, codeIntelLSPSymbolsName, decoded.Command)
			assert.Equal(t, wantQuery, decoded.Query)
			assert.False(t, decoded.Empty)
			assert.NotEmpty(t, decoded.LSPSymbols)

			return
		}

		assert.Contains(t, out.String(), "Run\tkind=12")
	})
}

func requireCodeIntelJSONFormat(t *testing.T, format string) {
	t.Helper()

	if format != outputFormatJSON {
		t.Fatalf("expected JSON output format, got %q", format)
	}
}

func codeIntelExampleMayBeEmpty(commandName string) bool {
	return commandName == codeIntelCyclesCommandName
}

func codeIntelStructuredOutputVariants(command string) []struct {
	name    string
	command string
} {
	return []struct {
		name    string
		command string
	}{
		{name: "json-flag", command: command + " --json"},
		{name: "output-json", command: command + " --output json"},
	}
}

func codeIntelLegacyFlagOutputVariants(descriptor codeIntelCommandDescriptor) []struct {
	name string
	args []string
} {
	base := codeIntelLegacyFlagArgs(descriptor)

	return []struct {
		name string
		args []string
	}{
		{name: "json-flag", args: append(append([]string(nil), base...), "--json")},
		{name: "output-json", args: append(append([]string(nil), base...), "--output", "json")},
	}
}

func codeIntelLegacyFlagArgs(descriptor codeIntelCommandDescriptor) []string {
	return append([]string{descriptor.LegacyFlag}, codeIntelExampleArgsForDescriptor(descriptor)...)
}

func codeIntelTextExampleArgsForTest(t *testing.T, command string) []string {
	t.Helper()

	args := splitCommandLineForTest(t, command)
	require.NotEmpty(t, args)
	require.Equal(t, "atteler", args[0])

	textArgs := make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		switch {
		case args[i] == "--json":
			continue
		case args[i] == "--output" && i+1 < len(args) && args[i+1] == outputFormatJSON:
			i++
			continue
		default:
			textArgs = append(textArgs, args[i])
		}
	}

	return textArgs
}
