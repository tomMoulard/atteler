package sdk_test

import (
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tommoulard/atteler/pkg/sdk"
)

func TestPackageContracts_AreDocumentedAndWellFormed(t *testing.T) {
	t.Parallel()

	contracts := sdk.PackageContracts()
	require.NotEmpty(t, contracts)

	docs := readRepoFile(t, "docs", "sdk.md")
	seen := make(map[string]struct{}, len(contracts))

	for i := range contracts {
		contract := contracts[i]
		require.NotEmpty(t, contract.ImportPath)
		require.NotEmpty(t, contract.Since)
		require.NotEmpty(t, contract.Summary)
		assert.Contains(t, docs, shortPackagePath(contract.ImportPath))

		switch contract.Stability {
		case sdk.StabilityStable, sdk.StabilityExperimental:
		default:
			require.Failf(t, "unknown stability", "contract %s has stability %q", contract.ImportPath, contract.Stability)
		}

		if _, ok := seen[contract.ImportPath]; ok {
			require.Failf(t, "duplicate package contract", "package %s appears more than once", contract.ImportPath)
		}

		seen[contract.ImportPath] = struct{}{}
	}
}

func TestPackageContracts_CoverEveryTopLevelPackage(t *testing.T) {
	t.Parallel()

	contracts := sdk.PackageContracts()

	contracted := make(map[string]struct{}, len(contracts))
	for i := range contracts {
		contracted[shortPackagePath(contracts[i].ImportPath)] = struct{}{}
	}

	entries, err := os.ReadDir(filepath.Join(repoRoot(t), "pkg"))
	require.NoError(t, err)

	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}

		packagePath := filepath.Join("pkg", entry.Name())
		assert.Contains(t, contracted, packagePath)
	}
}

func TestPackageContracts_JSONContractUsesStableFieldNames(t *testing.T) {
	t.Parallel()

	data, err := json.Marshal(sdk.PackageContracts()[:1])
	require.NoError(t, err)

	assert.JSONEq(t, `[{
		"import_path": "github.com/tommoulard/atteler/pkg/sdk",
		"stability": "stable",
		"since": "v0.1.0",
		"summary": "stable facade for common SDK workflows"
	}]`, string(data))
}

func TestAPIContract_JSONContractUsesStableEnvelope(t *testing.T) {
	t.Parallel()

	contract := sdk.APIContract()
	require.Equal(t, sdk.APIContractSchemaVersion, contract.SchemaVersion)
	require.Equal(t, sdk.CompatibilityPolicy, contract.CompatibilityPolicy)
	require.NotEmpty(t, contract.Packages)

	data, err := json.Marshal(contract)
	require.NoError(t, err)

	var envelope map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(data, &envelope))

	assert.Contains(t, envelope, "schema_version")
	assert.Contains(t, envelope, "compatibility_policy")
	assert.Contains(t, envelope, "packages")
	assert.Len(t, envelope, 3)
}

func TestAPIContract_DocsMentionMachineReadablePolicy(t *testing.T) {
	t.Parallel()

	docs := readRepoFile(t, "docs", "sdk.md")

	assert.Contains(t, docs, "pkg/sdk.APIContract()")
	assert.Contains(t, docs, "pkg/sdk.CompatibilityPolicy")
	assert.Contains(t, docs, "Release compatibility checklist")
	assert.Contains(t, docs, "deprecation window, migration path")
}

func TestSDKDocsMentionExportedFacadeIdentifiers(t *testing.T) {
	t.Parallel()

	docs := readRepoFile(t, "docs", "sdk.md")
	for _, name := range exportedSDKIdentifiers(t) {
		assert.Contains(t, docs, "`"+name+"`", "docs/sdk.md should mention exported pkg/sdk identifier %s", name)
	}
}

func TestExamplesDirectory_CoversDocumentedSDKWorkflows(t *testing.T) {
	t.Parallel()

	docs := readRepoFile(t, "docs", "sdk.md")
	for _, dir := range documentedExampleDirs() {
		examplePath := filepath.Join(repoRoot(t), "examples", dir, "main.go")
		require.FileExists(t, examplePath)
		assert.Contains(t, docs, "examples/"+dir)
	}
}

func TestExamplesDoNotImportCLIInternals(t *testing.T) {
	t.Parallel()

	for _, dir := range documentedExampleDirs() {
		path := filepath.Join(repoRoot(t), "examples", dir, "main.go")
		source := readRepoFile(t, "examples", dir, "main.go")

		assert.NotContains(t, source, "github.com/tommoulard/atteler/cmd", path)
		assert.NotContains(t, source, "cmd/atteler", path)
	}
}

func documentedExampleDirs() []string {
	return []string{
		"one-shot-chat",
		"provider-registry",
		"review-run",
		"memory-search",
		"plugin-execution",
		"worktree-session",
	}
}

func shortPackagePath(importPath string) string {
	const prefix = "github.com/tommoulard/atteler/"

	return strings.TrimPrefix(importPath, prefix)
}

func readRepoFile(t *testing.T, parts ...string) string {
	t.Helper()

	data, err := os.ReadFile(filepath.Join(append([]string{repoRoot(t)}, parts...)...))
	require.NoError(t, err)

	return string(data)
}

func exportedSDKIdentifiers(t *testing.T) []string {
	t.Helper()

	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, filepath.Join(repoRoot(t), "pkg", "sdk", "sdk.go"), nil, 0)
	require.NoError(t, err)

	seen := make(map[string]struct{})

	for _, decl := range file.Decls {
		switch decl := decl.(type) {
		case *ast.GenDecl:
			for _, spec := range decl.Specs {
				switch spec := spec.(type) {
				case *ast.TypeSpec:
					if ast.IsExported(spec.Name.Name) {
						seen[spec.Name.Name] = struct{}{}
					}
				case *ast.ValueSpec:
					for _, name := range spec.Names {
						if ast.IsExported(name.Name) {
							seen[name.Name] = struct{}{}
						}
					}
				}
			}
		case *ast.FuncDecl:
			if ast.IsExported(decl.Name.Name) {
				seen[decl.Name.Name] = struct{}{}
			}
		}
	}

	names := make([]string, 0, len(seen))
	for name := range seen {
		names = append(names, name)
	}

	sort.Strings(names)

	return names
}

func repoRoot(t *testing.T) string {
	t.Helper()

	_, filename, _, ok := runtime.Caller(0)
	require.True(t, ok)

	return filepath.Clean(filepath.Join(filepath.Dir(filename), "..", ".."))
}
