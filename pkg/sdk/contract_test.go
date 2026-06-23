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
	"strconv"
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
		case sdk.StabilityStable:
			assert.NotEmpty(t, contract.PrimaryIdentifiers, "stable package %s should publish primary identifiers", contract.ImportPath)
		case sdk.StabilityExperimental:
			assert.Empty(t, contract.PrimaryIdentifiers, "experimental package %s should not publish stable primary identifiers", contract.ImportPath)
		default:
			require.Failf(t, "unknown stability", "contract %s has stability %q", contract.ImportPath, contract.Stability)
		}

		assertNoDuplicateStrings(t, contract.ImportPath+" primary identifiers", contract.PrimaryIdentifiers)

		if _, ok := seen[contract.ImportPath]; ok {
			require.Failf(t, "duplicate package contract", "package %s appears more than once", contract.ImportPath)
		}

		seen[contract.ImportPath] = struct{}{}
	}
}

func TestPackageContracts_DocsKeepStabilitySectionsAligned(t *testing.T) {
	t.Parallel()

	docs := readRepoFile(t, "docs", "sdk.md")
	stableSection := sectionBetween(t, docs, "## Stable packages and primary types", "## Experimental packages")
	experimentalSection := sectionBetween(t, docs, "## Experimental packages", "## Runnable examples")

	for _, contract := range sdk.PackageContracts() {
		shortPath := shortPackagePath(contract.ImportPath)
		switch contract.Stability {
		case sdk.StabilityStable:
			assert.Contains(t, stableSection, "`"+shortPath+"`")
			assert.NotContains(t, experimentalSection, "`"+shortPath+"`")
		case sdk.StabilityExperimental:
			assert.Contains(t, experimentalSection, "`"+shortPath+"`")
			assert.NotContains(t, stableSection, "`"+shortPath+"`")
		}
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
		"summary": "stable facade for common SDK workflows",
		"primary_identifiers": [
			"RunOneShotChat",
			"NewProviderRegistry",
			"BuildMemoryIndex",
			"NewReviewRun",
			"RunPlugin",
			"NewSession",
			"AttachNewWorktree",
			"APIContract",
			"CompatibilityPolicy",
			"Contract",
			"PackageContract",
			"Stability",
			"PackageContracts",
			"PackagesByStability"
		]
	}]`, string(data))
}

func TestPackageContracts_PrimaryIdentifiersAreDocumented(t *testing.T) {
	t.Parallel()

	docs := readRepoFile(t, "docs", "sdk.md")

	for _, contract := range sdk.PackagesByStability(sdk.StabilityStable) {
		require.NotEmpty(t, contract.PrimaryIdentifiers, "%s should declare primary identifiers", contract.ImportPath)

		for _, identifier := range contract.PrimaryIdentifiers {
			assert.Contains(t, docs, "`"+identifier+"`", "%s primary identifier %s should be documented", contract.ImportPath, identifier)
		}
	}
}

func TestPackageContracts_PrimaryIdentifiersExistInPackages(t *testing.T) {
	t.Parallel()

	for _, contract := range sdk.PackagesByStability(sdk.StabilityStable) {
		packagePath := shortPackagePath(contract.ImportPath)
		identifiers := exportedPackageIdentifiers(t, packagePath)

		for _, identifier := range contract.PrimaryIdentifiers {
			assert.Contains(t, identifiers, identifier, "%s should export primary identifier %s", packagePath, identifier)
		}
	}
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

func TestAPIContract_ReturnsPackageCopy(t *testing.T) {
	t.Parallel()

	contract := sdk.APIContract()
	require.NotEmpty(t, contract.Packages)

	contract.Packages[0].ImportPath = "mutated"
	require.NotEmpty(t, contract.Packages[0].PrimaryIdentifiers)
	contract.Packages[0].PrimaryIdentifiers[0] = "MutatedIdentifier"

	again := sdk.APIContract()
	require.NotEmpty(t, again.Packages)
	assert.NotEqual(t, "mutated", again.Packages[0].ImportPath)
	assert.NotEqual(t, "MutatedIdentifier", again.Packages[0].PrimaryIdentifiers[0])
}

func TestAPIContract_DocsMentionMachineReadablePolicy(t *testing.T) {
	t.Parallel()

	docs := readRepoFile(t, "docs", "sdk.md")

	assert.Contains(t, docs, "pkg/sdk.APIContract()")
	assert.Contains(t, docs, "pkg/sdk.CompatibilityPolicy")
	assert.Contains(t, docs, "primary_identifiers")
	assert.Contains(t, docs, "experimental rows intentionally omit")
	assert.Contains(t, docs, "Release compatibility checklist")
	assert.Contains(t, docs, "deprecation window, migration path")
	assert.Contains(t, docs, "go test ./pkg/sdk ./pkg/review")
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
	examplesReadme := readRepoFile(t, "examples", "README.md")

	for _, dir := range documentedExampleDirs() {
		examplePath := filepath.Join(repoRoot(t), "examples", dir, "main.go")
		require.FileExists(t, examplePath)
		assert.Contains(t, docs, "examples/"+dir)
		assert.Contains(t, examplesReadme, "`"+dir+"`")
	}
}

func TestExamplesDirectory_HasNoUndocumentedWorkflowDirs(t *testing.T) {
	t.Parallel()

	entries, err := os.ReadDir(filepath.Join(repoRoot(t), "examples"))
	require.NoError(t, err)

	var actualDirs []string

	for _, entry := range entries {
		if entry.IsDir() {
			actualDirs = append(actualDirs, entry.Name())
		}
	}

	assert.ElementsMatch(t, documentedExampleDirs(), actualDirs)
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

func TestExamplesOnlyImportStableSDKPackages(t *testing.T) {
	t.Parallel()

	stableImports := make(map[string]struct{})
	for _, contract := range sdk.PackagesByStability(sdk.StabilityStable) {
		stableImports[contract.ImportPath] = struct{}{}
	}

	for _, dir := range documentedExampleDirs() {
		examplePath := filepath.Join(repoRoot(t), "examples", dir, "main.go")
		file := parseGoFile(t, examplePath)

		for _, spec := range file.Imports {
			importPath, err := strconv.Unquote(spec.Path.Value)
			require.NoError(t, err)

			if strings.HasPrefix(importPath, "github.com/tommoulard/atteler/cmd") {
				require.Failf(t, "example imports CLI internals", "%s imports %s", examplePath, importPath)
			}

			if strings.HasPrefix(importPath, "github.com/tommoulard/atteler/pkg/") {
				assert.Contains(t, stableImports, importPath, "%s should only import stable SDK packages", examplePath)
			}
		}
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

func assertNoDuplicateStrings(t *testing.T, label string, values []string) {
	t.Helper()

	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		if _, ok := seen[value]; ok {
			require.Failf(t, "duplicate value", "%s contains duplicate %q", label, value)
		}

		seen[value] = struct{}{}
	}
}

func shortPackagePath(importPath string) string {
	const prefix = "github.com/tommoulard/atteler/"

	return strings.TrimPrefix(importPath, prefix)
}

func sectionBetween(t *testing.T, source, start, end string) string {
	t.Helper()

	startIndex := strings.Index(source, start)
	require.NotEqual(t, -1, startIndex, "section start %q not found", start)

	section := source[startIndex+len(start):]
	endIndex := strings.Index(section, end)
	require.NotEqual(t, -1, endIndex, "section end %q not found", end)

	return section[:endIndex]
}

func readRepoFile(t *testing.T, parts ...string) string {
	t.Helper()

	data, err := os.ReadFile(filepath.Join(append([]string{repoRoot(t)}, parts...)...))
	require.NoError(t, err)

	return string(data)
}

func exportedSDKIdentifiers(t *testing.T) []string {
	t.Helper()

	file := parseGoFile(t, filepath.Join(repoRoot(t), "pkg", "sdk", "sdk.go"))

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

func exportedPackageIdentifiers(t *testing.T, packagePath string) map[string]struct{} {
	t.Helper()

	identifiers := make(map[string]struct{})
	root := filepath.Join(repoRoot(t), packagePath)

	entries, err := os.ReadDir(root)
	require.NoError(t, err)

	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".go") || strings.HasSuffix(entry.Name(), "_test.go") {
			continue
		}

		file := parseGoFile(t, filepath.Join(root, entry.Name()))
		for _, decl := range file.Decls {
			switch decl := decl.(type) {
			case *ast.GenDecl:
				for _, spec := range decl.Specs {
					switch spec := spec.(type) {
					case *ast.TypeSpec:
						if ast.IsExported(spec.Name.Name) {
							identifiers[spec.Name.Name] = struct{}{}
						}
					case *ast.ValueSpec:
						for _, name := range spec.Names {
							if ast.IsExported(name.Name) {
								identifiers[name.Name] = struct{}{}
							}
						}
					}
				}
			case *ast.FuncDecl:
				if ast.IsExported(decl.Name.Name) {
					identifiers[decl.Name.Name] = struct{}{}
				}
			}
		}
	}

	return identifiers
}

func parseGoFile(t *testing.T, path string) *ast.File {
	t.Helper()

	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, path, nil, 0)
	require.NoError(t, err)

	return file
}

func repoRoot(t *testing.T) string {
	t.Helper()

	_, filename, _, ok := runtime.Caller(0)
	require.True(t, ok)

	return filepath.Clean(filepath.Join(filepath.Dir(filename), "..", ".."))
}
