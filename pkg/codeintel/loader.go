//nolint:govet,wsl_v5 // Public metadata structs prioritize stable/readable field order; wsl is noisy for evidence-building code.
package codeintel

import (
	"bytes"
	"context"
	"fmt"
	"go/ast"
	"go/parser"
	"go/printer"
	"go/token"
	"go/types"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"

	"golang.org/x/tools/go/packages"

	"github.com/tommoulard/atteler/pkg/codegraph"
)

type loadRequest struct {
	Context      context.Context
	Dir          string
	Patterns     []string
	Options      IndexOptions
	Fingerprints map[string]fileFingerprint
}

type includedFile struct {
	pkg         *packages.Package
	path        string
	syntax      *ast.File
	build       BuildContext
	fingerprint fileFingerprint
	generated   bool
	test        bool
}

type declarationSpan struct {
	file  string
	start token.Pos
	end   token.Pos
	id    string
}

type semanticBuilder struct {
	fset             *token.FileSet
	options          IndexOptions
	fingerprints     map[string]fileFingerprint
	index            Index
	graph            *codegraph.EvidenceGraph
	objectDecls      map[types.Object]string
	objectDeclsByKey map[string]string
	included         []includedFile
	declarationSpans []declarationSpan
	declIDsByNamePos map[token.Pos]string
}

func loadIndex(req loadRequest) (Index, error) {
	graph := codegraph.NewEvidence()
	if len(req.Patterns) == 0 {
		return Index{Graph: graph}, nil
	}

	fset := token.NewFileSet()
	cfg := &packages.Config{
		Mode:       packages.NeedName | packages.NeedFiles | packages.NeedCompiledGoFiles | packages.NeedSyntax | packages.NeedTypes | packages.NeedTypesInfo | packages.NeedImports | packages.NeedModule,
		Context:    req.Context,
		Dir:        req.Dir,
		Env:        loaderEnv(req.Options.Env),
		BuildFlags: buildFlags(req.Options),
		Fset:       fset,
		Tests:      !req.Options.ExcludeTests,
		ParseFile: func(fset *token.FileSet, filename string, src []byte) (*ast.File, error) {
			return parser.ParseFile(fset, filename, src, parser.ParseComments)
		},
	}

	pkgs, err := packages.Load(cfg, req.Patterns...)
	if err != nil {
		return Index{}, fmt.Errorf("load packages: %w", err)
	}

	sortPackages(pkgs)
	diagnostics, err := collectDiagnostics(pkgs)
	if err != nil {
		return Index{}, err
	}

	builder := semanticBuilder{
		fset:             fset,
		options:          req.Options,
		fingerprints:     req.Fingerprints,
		index:            Index{Graph: graph, Diagnostics: diagnostics},
		graph:            graph,
		objectDecls:      make(map[types.Object]string),
		objectDeclsByKey: make(map[string]string),
		declIDsByNamePos: make(map[token.Pos]string),
	}

	builder.collectPackages(pkgs)
	builder.collectReferencesAndCalls()
	builder.finish()

	return builder.index, nil
}

func loaderEnv(extra []string) []string {
	if len(extra) == 0 {
		return nil
	}

	return append(os.Environ(), extra...)
}

func buildFlags(opts IndexOptions) []string {
	if len(opts.Tags) == 0 {
		return nil
	}

	tags := append([]string(nil), opts.Tags...)
	sort.Strings(tags)

	return []string{"-tags=" + strings.Join(tags, ",")}
}

func sortPackages(pkgs []*packages.Package) {
	sort.Slice(pkgs, func(i, j int) bool {
		if pkgs[i].ID != pkgs[j].ID {
			return pkgs[i].ID < pkgs[j].ID
		}

		return pkgs[i].PkgPath < pkgs[j].PkgPath
	})
}

func collectDiagnostics(pkgs []*packages.Package) ([]Diagnostic, error) {
	var diagnostics []Diagnostic
	for _, pkg := range pkgs {
		for _, pkgErr := range pkg.Errors {
			if ignorablePackageError(pkgErr) {
				continue
			}

			diagnostic := Diagnostic{
				PackageID: pkg.ID,
				Position:  pkgErr.Pos,
				Message:   pkgErr.Msg,
				Kind:      packageErrorKind(pkgErr.Kind),
			}
			diagnostics = append(diagnostics, diagnostic)

			if pkgErr.Kind == packages.ParseError {
				return diagnostics, fmt.Errorf("parse %s: %s", pkgErr.Pos, pkgErr.Msg)
			}
		}
	}

	return diagnostics, nil
}

func ignorablePackageError(pkgErr packages.Error) bool {
	return strings.Contains(pkgErr.Msg, "loading compiled Go files from cache") && strings.Contains(pkgErr.Msg, "cache entry not found")
}

func packageErrorKind(kind packages.ErrorKind) string {
	switch kind {
	case packages.ListError:
		return "list"
	case packages.ParseError:
		return "parse"
	case packages.TypeError:
		return "type"
	default:
		return "unknown"
	}
}

func (builder *semanticBuilder) collectPackages(pkgs []*packages.Package) {
	packagesByID := make(map[string]*PackageInfo)

	for _, pkg := range pkgs {
		if skipLoadedPackage(pkg) {
			continue
		}

		files := builder.includedPackageFiles(pkg)
		if len(files) == 0 {
			continue
		}

		info := packagesByID[pkg.ID]
		if info == nil {
			info = &PackageInfo{
				ID:         pkg.ID,
				Name:       pkg.Name,
				Path:       pkg.PkgPath,
				ModulePath: modulePath(pkg),
				Test:       isTestPackage(pkg),
			}
			packagesByID[pkg.ID] = info
		}

		pkgNode := packageNodeID(pkg.ID)
		builder.graph.AddNode(codegraph.Node{ID: pkgNode, Kind: "package", Name: packageDisplayName(pkg)})
		apiNode := packageAPINodeID(pkg.ID)
		pkgProvenance := Provenance{
			Source:     "go/packages",
			Build:      buildContext(builder.options, pkg, isTestPackage(pkg), false),
			Confidence: "high",
		}
		builder.graph.AddNode(codegraph.Node{ID: apiNode, Kind: "api_boundary", Name: packageDisplayName(pkg)})
		builder.graph.AddRelationship(codegraph.Relationship{
			From:       pkgNode,
			To:         apiNode,
			Kind:       "has_api_boundary",
			Provenance: []codegraph.Provenance{graphProvenance(pkgProvenance)},
		})

		for i := range files {
			info.Files = append(info.Files, files[i].path)
			builder.collectFile(files[i], pkgNode)
		}
	}

	ids := make([]string, 0, len(packagesByID))
	for id := range packagesByID {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		info := *packagesByID[id]
		sort.Strings(info.Files)
		builder.index.Packages = append(builder.index.Packages, info)
	}
}

func skipLoadedPackage(pkg *packages.Package) bool {
	if pkg == nil || len(pkg.Syntax) == 0 {
		return true
	}
	if strings.HasSuffix(pkg.PkgPath, ".test") && pkg.Name == "main" {
		return true
	}

	return false
}

func (builder *semanticBuilder) includedPackageFiles(pkg *packages.Package) []includedFile {
	var files []includedFile
	for i, syntax := range pkg.Syntax {
		path := syntaxFilePath(builder.fset, pkg, syntax, i)
		fingerprint, ok := builder.fingerprints[path]
		if !ok {
			continue
		}

		test := isTestFile(path)
		if builder.options.ExcludeTests && test {
			continue
		}
		if isTestPackage(pkg) && !test {
			continue
		}

		generated := ast.IsGenerated(syntax)
		if builder.options.ExcludeGenerated && generated {
			continue
		}

		build := buildContext(builder.options, pkg, test, generated)
		files = append(files, includedFile{
			pkg:         pkg,
			path:        path,
			syntax:      syntax,
			build:       build,
			fingerprint: fingerprint,
			generated:   generated,
			test:        test,
		})
	}

	sort.Slice(files, func(i, j int) bool { return files[i].path < files[j].path })

	return files
}

func syntaxFilePath(fset *token.FileSet, pkg *packages.Package, file *ast.File, index int) string {
	if file != nil && file.Package.IsValid() {
		if filename := fset.Position(file.Package).Filename; filename != "" {
			return cleanPath(filename)
		}
	}
	if index < len(pkg.CompiledGoFiles) {
		return cleanPath(pkg.CompiledGoFiles[index])
	}
	if index < len(pkg.GoFiles) {
		return cleanPath(pkg.GoFiles[index])
	}

	return ""
}

func (builder *semanticBuilder) collectFile(file includedFile, pkgNode codegraph.NodeID) {
	builder.included = append(builder.included, file)

	fileNode := fileNodeID(file.path)
	fileRange := sourceRange(builder.fset, file.syntax)
	fileProvenance := Provenance{Source: "go/packages", Range: fileRange, Build: file.build, Confidence: "high"}

	builder.graph.AddNode(codegraph.Node{ID: fileNode, Kind: "file", Name: filepath.ToSlash(file.path)})
	builder.graph.AddRelationship(codegraph.Relationship{
		From:       pkgNode,
		To:         fileNode,
		Kind:       "contains",
		Provenance: []codegraph.Provenance{graphProvenance(fileProvenance)},
	})

	sourceFile := SourceFile{
		Path:        file.path,
		PackageName: file.syntax.Name.Name,
		PackageID:   file.pkg.ID,
		PackagePath: file.pkg.PkgPath,
		ModulePath:  modulePath(file.pkg),
		Generated:   file.generated,
		Test:        file.test,
		BuildTags:   buildTagExpressions(file.syntax),
		Range:       fileRange,
		ContentHash: file.fingerprint.Hash,
		ModTime:     file.fingerprint.ModTime,
		Build:       file.build,
		Provenance:  fileProvenance,
	}
	builder.index.FileDetails = append(builder.index.FileDetails, sourceFile)

	legacy := File{Path: file.path, Package: file.syntax.Name.Name}
	legacy.Imports, builder.index.Imports, builder.index.ImportEdges = builder.collectImports(file, fileNode, legacy.Imports, builder.index.Imports, builder.index.ImportEdges)
	legacy.Symbols = builder.collectDeclarations(file, fileNode)
	builder.index.Files = append(builder.index.Files, legacy)
}

func (builder *semanticBuilder) collectImports(file includedFile, fileNode codegraph.NodeID, legacyImports []string, imports []Import, edges []ImportEdge) ([]string, []Import, []ImportEdge) {
	for _, spec := range file.syntax.Imports {
		importPath, err := strconv.Unquote(spec.Path.Value)
		if err != nil {
			importPath = spec.Path.Value
		}

		legacyImports = append(legacyImports, importPath)
		edges = append(edges, ImportEdge{From: file.path, Import: importPath})

		alias := ""
		if spec.Name != nil {
			alias = spec.Name.Name
		}

		rangeInfo := sourceRange(builder.fset, spec.Path)
		build := file.build
		provenance := Provenance{Source: "parser:import", Range: rangeInfo, Build: build, Confidence: "high"}
		resolved := importPath
		if imported := file.pkg.Imports[importPath]; imported != nil && imported.PkgPath != "" {
			resolved = imported.PkgPath
		}

		semanticImport := Import{
			ID:                  importNodeID(file.path, rangeInfo, importPath),
			Path:                importPath,
			Alias:               alias,
			File:                file.path,
			PackageID:           file.pkg.ID,
			PackagePath:         file.pkg.PkgPath,
			ResolvedPackagePath: resolved,
			Range:               rangeInfo,
			Build:               build,
			Provenance:          provenance,
		}
		imports = append(imports, semanticImport)

		importNode := codegraph.NodeID(semanticImport.ID)
		builder.graph.AddNode(codegraph.Node{ID: importNode, Kind: "import", Name: importPath})
		builder.graph.AddRelationship(codegraph.Relationship{
			From:       fileNode,
			To:         importNode,
			Kind:       "imports",
			Provenance: []codegraph.Provenance{graphProvenance(provenance)},
		})

		importedPackageNode := packageNodeID(resolved)
		builder.graph.AddNode(codegraph.Node{ID: importedPackageNode, Kind: "package", Name: resolved})
		builder.graph.AddRelationship(codegraph.Relationship{
			From:       importNode,
			To:         importedPackageNode,
			Kind:       "resolves_to",
			Provenance: []codegraph.Provenance{graphProvenance(provenance)},
		})
		builder.graph.AddRelationship(codegraph.Relationship{
			From:       packageNodeID(file.pkg.ID),
			To:         importedPackageNode,
			Kind:       "imports_package",
			Provenance: []codegraph.Provenance{graphProvenance(provenance)},
		})
	}

	sort.Strings(legacyImports)

	return legacyImports, imports, edges
}

func (builder *semanticBuilder) collectDeclarations(file includedFile, fileNode codegraph.NodeID) []Symbol {
	var symbols []Symbol
	for _, decl := range file.syntax.Decls {
		switch decl := decl.(type) {
		case *ast.FuncDecl:
			symbols = append(symbols, builder.collectFuncDeclaration(file, fileNode, decl))
		case *ast.GenDecl:
			symbols = append(symbols, builder.collectGenDeclarations(file, fileNode, decl)...)
		}
	}

	return symbols
}

func (builder *semanticBuilder) collectFuncDeclaration(file includedFile, fileNode codegraph.NodeID, decl *ast.FuncDecl) Symbol {
	symbol, declaration := builder.funcDeclaration(file, decl)
	builder.addDeclaration(fileNode, declaration, decl.Pos(), decl.End(), decl.Name.Pos())

	return symbol
}

func (builder *semanticBuilder) collectGenDeclarations(file includedFile, fileNode codegraph.NodeID, decl *ast.GenDecl) []Symbol {
	var symbols []Symbol
	for _, spec := range decl.Specs {
		switch spec := spec.(type) {
		case *ast.TypeSpec:
			symbols = append(symbols, builder.collectTypeDeclaration(file, fileNode, spec))
		case *ast.ValueSpec:
			symbols = append(symbols, builder.collectValueDeclarations(file, fileNode, spec, tokenKind(decl.Tok))...)
		}
	}

	return symbols
}

func (builder *semanticBuilder) collectTypeDeclaration(file includedFile, fileNode codegraph.NodeID, spec *ast.TypeSpec) Symbol {
	symbol, declaration := builder.namedDeclaration(file, spec.Name, "type", "")
	builder.addDeclaration(fileNode, declaration, spec.Pos(), spec.End(), spec.Name.Pos())

	return symbol
}

func (builder *semanticBuilder) collectValueDeclarations(file includedFile, fileNode codegraph.NodeID, spec *ast.ValueSpec, kind string) []Symbol {
	symbols := make([]Symbol, 0, len(spec.Names))
	for _, name := range spec.Names {
		if name.Name == "_" {
			continue
		}

		symbol, declaration := builder.namedDeclaration(file, name, kind, "")
		symbols = append(symbols, symbol)
		builder.addDeclaration(fileNode, declaration, spec.Pos(), spec.End(), name.Pos())
	}

	return symbols
}

func (builder *semanticBuilder) funcDeclaration(file includedFile, decl *ast.FuncDecl) (Symbol, Declaration) {
	kind := kindFunc
	receiver := ""
	if decl.Recv != nil {
		kind = kindMethod
		receiver = receiverName(decl.Recv)
	}

	return builder.namedDeclaration(file, decl.Name, kind, receiver)
}

func (builder *semanticBuilder) namedDeclaration(file includedFile, name *ast.Ident, kind, receiver string) (Symbol, Declaration) {
	position := builder.fset.Position(name.Pos())
	rangeInfo := sourceRangeFromPos(builder.fset, name.Pos(), name.End())
	id := declarationID(file.pkg, name.Name, kind, receiver, rangeInfo)
	build := file.build
	provenance := Provenance{Source: "types:def", Range: rangeInfo, Build: build, Confidence: "high"}

	if object := file.pkg.TypesInfo.Defs[name]; object != nil {
		builder.objectDecls[object] = id
		if key := objectKey(object); key != "" {
			builder.objectDeclsByKey[key] = id
		}
	}

	return Symbol{Name: name.Name, Kind: kind, File: file.path, Line: position.Line}, Declaration{
		ID:          id,
		Name:        name.Name,
		Kind:        kind,
		PackageName: file.syntax.Name.Name,
		PackageID:   file.pkg.ID,
		PackagePath: file.pkg.PkgPath,
		Receiver:    receiver,
		File:        file.path,
		Range:       rangeInfo,
		Exported:    exportedDeclaration(name.Name, receiver),
		Build:       build,
		Provenance:  provenance,
	}
}

func (builder *semanticBuilder) addDeclaration(fileNode codegraph.NodeID, declaration Declaration, start, end, namePos token.Pos) {
	builder.index.Declarations = append(builder.index.Declarations, declaration)
	builder.index.Symbols = append(builder.index.Symbols, Symbol{Name: declaration.Name, Kind: declaration.Kind, File: declaration.File, Line: declaration.Range.StartLine})
	builder.declarationSpans = append(builder.declarationSpans, declarationSpan{file: declaration.File, start: start, end: end, id: declaration.ID})
	builder.declIDsByNamePos[namePos] = declaration.ID

	declNode := codegraph.NodeID(declaration.ID)
	builder.graph.AddNode(codegraph.Node{ID: declNode, Kind: "declaration", Name: declaration.Name})
	builder.graph.AddRelationship(codegraph.Relationship{
		From:       fileNode,
		To:         declNode,
		Kind:       "declares",
		Provenance: []codegraph.Provenance{graphProvenance(declaration.Provenance)},
	})
	if declaration.Exported {
		builder.graph.AddRelationship(codegraph.Relationship{
			From:       packageAPINodeID(declaration.PackageID),
			To:         declNode,
			Kind:       "exports",
			Provenance: []codegraph.Provenance{graphProvenance(declaration.Provenance)},
		})
		builder.graph.AddRelationship(codegraph.Relationship{
			From:       packageNodeID(declaration.PackageID),
			To:         declNode,
			Kind:       "exports",
			Provenance: []codegraph.Provenance{graphProvenance(declaration.Provenance)},
		})
	}
}

func (builder *semanticBuilder) collectReferencesAndCalls() {
	for i := range builder.included {
		builder.collectReferences(builder.included[i])
		builder.collectCalls(builder.included[i])
	}
}

func (builder *semanticBuilder) collectReferences(file includedFile) {
	for ident, object := range file.pkg.TypesInfo.Uses {
		if object == nil || ident.Name == "_" {
			continue
		}
		if cleanPath(builder.fset.Position(ident.Pos()).Filename) != file.path {
			continue
		}

		rangeInfo := sourceRangeFromPos(builder.fset, ident.Pos(), ident.End())
		toID := builder.declarationIDForObject(object)
		fromID := builder.enclosingDeclarationID(file.path, ident.Pos())
		kind := objectKind(object)
		build := file.build
		provenance := Provenance{Source: "types:use", Range: rangeInfo, Build: build, Confidence: referenceConfidence(toID)}
		reference := Reference{
			ID:                referenceNodeID(file.path, rangeInfo, ident.Name),
			Name:              ident.Name,
			Kind:              kind,
			File:              file.path,
			FromPackageID:     file.pkg.ID,
			FromDeclarationID: fromID,
			ToDeclarationID:   toID,
			Range:             rangeInfo,
			Build:             build,
			Provenance:        provenance,
		}
		builder.index.References = append(builder.index.References, reference)

		refNode := codegraph.NodeID(reference.ID)
		builder.graph.AddNode(codegraph.Node{ID: refNode, Kind: "reference", Name: reference.Name})
		fromNode := fileNodeID(file.path)
		if fromID != "" {
			fromNode = codegraph.NodeID(fromID)
		}
		builder.graph.AddRelationship(codegraph.Relationship{
			From:       fromNode,
			To:         refNode,
			Kind:       "contains_reference",
			Provenance: []codegraph.Provenance{graphProvenance(provenance)},
		})
		builder.graph.AddNode(codegraph.Node{ID: codegraph.NodeID(toID), Kind: declarationNodeKind(toID), Name: reference.Name})
		builder.graph.AddRelationship(codegraph.Relationship{
			From:       refNode,
			To:         codegraph.NodeID(toID),
			Kind:       "references",
			Provenance: []codegraph.Provenance{graphProvenance(provenance)},
		})
	}
}

func (builder *semanticBuilder) collectCalls(file includedFile) {
	for _, decl := range file.syntax.Decls {
		funcDecl, ok := decl.(*ast.FuncDecl)
		if !ok || funcDecl.Body == nil {
			continue
		}

		callerID := builder.declIDsByNamePos[funcDecl.Name.Pos()]
		if callerID == "" {
			continue
		}

		ast.Inspect(funcDecl.Body, func(node ast.Node) bool {
			call, ok := node.(*ast.CallExpr)
			if !ok {
				return true
			}

			object := callObject(file.pkg.TypesInfo, call)
			if object == nil {
				return true
			}

			kind := objectKind(object)
			if kind != kindFunc && kind != kindMethod && kind != kindBuiltin {
				return true
			}

			calleeID := builder.declarationIDForObject(object)
			rangeInfo := sourceRangeFromPos(builder.fset, call.Fun.Pos(), call.Rparen)
			provenance := Provenance{Source: "types:call", Range: rangeInfo, Build: file.build, Confidence: referenceConfidence(calleeID)}
			edge := CallEdge{
				CallerID:   callerID,
				CalleeID:   calleeID,
				File:       file.path,
				Range:      rangeInfo,
				Build:      file.build,
				Provenance: provenance,
			}
			builder.index.CallEdges = append(builder.index.CallEdges, edge)
			builder.graph.AddNode(codegraph.Node{ID: codegraph.NodeID(calleeID), Kind: declarationNodeKind(calleeID), Name: object.Name()})
			builder.graph.AddRelationship(codegraph.Relationship{
				From:       codegraph.NodeID(callerID),
				To:         codegraph.NodeID(calleeID),
				Kind:       "calls",
				Provenance: []codegraph.Provenance{graphProvenance(provenance)},
			})

			return true
		})
	}
}

func (builder *semanticBuilder) declarationIDForObject(object types.Object) string {
	if id := builder.objectDecls[object]; id != "" {
		return id
	}
	if id := builder.objectDeclsByKey[objectKey(object)]; id != "" {
		return id
	}

	pkgPath := ""
	if object.Pkg() != nil {
		pkgPath = object.Pkg().Path()
	}
	if pkgPath == "" {
		pkgPath = "builtin"
	}

	return "external:" + pkgPath + ":" + strings.ReplaceAll(objectKey(object), "\x00", ":")
}

func (builder *semanticBuilder) enclosingDeclarationID(file string, pos token.Pos) string {
	var match string
	var matchSize int
	for _, span := range builder.declarationSpans {
		if span.file != file || pos < span.start || pos > span.end {
			continue
		}

		size := int(span.end - span.start)
		if match == "" || size < matchSize {
			match = span.id
			matchSize = size
		}
	}

	return match
}

func (builder *semanticBuilder) finish() {
	sort.Slice(builder.index.Files, func(i, j int) bool { return builder.index.Files[i].Path < builder.index.Files[j].Path })
	sort.Slice(builder.index.FileDetails, func(i, j int) bool { return builder.index.FileDetails[i].Path < builder.index.FileDetails[j].Path })
	sort.Slice(builder.index.Declarations, func(i, j int) bool {
		return declarationLess(builder.index.Declarations[i], builder.index.Declarations[j])
	})
	sort.Slice(builder.index.Imports, func(i, j int) bool { return importLess(builder.index.Imports[i], builder.index.Imports[j]) })
	sort.Slice(builder.index.References, func(i, j int) bool { return referenceLess(builder.index.References[i], builder.index.References[j]) })
	sort.Slice(builder.index.CallEdges, func(i, j int) bool { return callEdgeLess(builder.index.CallEdges[i], builder.index.CallEdges[j]) })
	sort.Slice(builder.index.ImportEdges, func(i, j int) bool {
		if builder.index.ImportEdges[i].From != builder.index.ImportEdges[j].From {
			return builder.index.ImportEdges[i].From < builder.index.ImportEdges[j].From
		}

		return builder.index.ImportEdges[i].Import < builder.index.ImportEdges[j].Import
	})
	sort.Slice(builder.index.Symbols, func(i, j int) bool {
		if builder.index.Symbols[i].Name != builder.index.Symbols[j].Name {
			return builder.index.Symbols[i].Name < builder.index.Symbols[j].Name
		}
		if builder.index.Symbols[i].File != builder.index.Symbols[j].File {
			return builder.index.Symbols[i].File < builder.index.Symbols[j].File
		}

		return builder.index.Symbols[i].Line < builder.index.Symbols[j].Line
	})
}

func modulePath(pkg *packages.Package) string {
	if pkg.Module == nil {
		return ""
	}

	return pkg.Module.Path
}

func packageDisplayName(pkg *packages.Package) string {
	if pkg.PkgPath != "" {
		return pkg.PkgPath
	}

	return pkg.ID
}

func isTestPackage(pkg *packages.Package) bool {
	return strings.Contains(pkg.ID, ".test]") || strings.Contains(pkg.ID, "[")
}

func isTestFile(path string) bool {
	return strings.HasSuffix(path, "_test.go")
}

func buildContext(opts IndexOptions, pkg *packages.Package, test, generated bool) BuildContext {
	tags := append([]string(nil), opts.Tags...)
	sort.Strings(tags)

	return BuildContext{
		GOOS:        envValue(opts.Env, "GOOS", runtime.GOOS),
		GOARCH:      envValue(opts.Env, "GOARCH", runtime.GOARCH),
		Tags:        tags,
		Test:        test,
		Generated:   generated,
		PackageID:   pkg.ID,
		PackagePath: pkg.PkgPath,
		ModulePath:  modulePath(pkg),
	}
}

func envValue(env []string, key, fallback string) string {
	prefix := key + "="
	for i := len(env) - 1; i >= 0; i-- {
		if value, ok := strings.CutPrefix(env[i], prefix); ok {
			return value
		}
	}
	if value := os.Getenv(key); value != "" {
		return value
	}

	return fallback
}

func buildTagExpressions(file *ast.File) []string {
	var tags []string
	for _, group := range file.Comments {
		if group.End() > file.Package {
			break
		}
		for _, comment := range group.List {
			text := strings.TrimSpace(comment.Text)
			switch {
			case strings.HasPrefix(text, "//go:build"):
				tags = append(tags, strings.TrimSpace(strings.TrimPrefix(text, "//go:build")))
			case strings.HasPrefix(text, "// +build"):
				tags = append(tags, strings.TrimSpace(strings.TrimPrefix(text, "// +build")))
			}
		}
	}

	return tags
}

func sourceRange(fset *token.FileSet, node ast.Node) SourceRange {
	if node == nil {
		return SourceRange{}
	}

	return sourceRangeFromPos(fset, node.Pos(), node.End())
}

func sourceRangeFromPos(fset *token.FileSet, start, end token.Pos) SourceRange {
	startPos := fset.Position(start)
	endPos := fset.Position(end)
	return SourceRange{
		File:        cleanPath(startPos.Filename),
		StartLine:   startPos.Line,
		StartColumn: startPos.Column,
		EndLine:     endPos.Line,
		EndColumn:   endPos.Column,
	}
}

func exportedDeclaration(name, receiver string) bool {
	if !ast.IsExported(name) {
		return false
	}
	if receiver == "" {
		return true
	}

	return ast.IsExported(receiverBaseName(receiver))
}

func receiverName(recv *ast.FieldList) string {
	if recv == nil || len(recv.List) == 0 {
		return ""
	}

	var buf bytes.Buffer
	if err := printer.Fprint(&buf, token.NewFileSet(), recv.List[0].Type); err != nil {
		return ""
	}

	return receiverBaseName(buf.String())
}

func receiverBaseName(receiver string) string {
	receiver = strings.TrimPrefix(receiver, "*")
	if base, _, ok := strings.Cut(receiver, "["); ok {
		return base
	}

	return receiver
}

func declarationID(pkg *packages.Package, name, kind, receiver string, rangeInfo SourceRange) string {
	pkgPath := pkg.PkgPath
	if pkgPath == "" {
		pkgPath = pkg.ID
	}

	qualified := name
	if receiver != "" {
		qualified = receiver + "." + name
	}

	return strings.Join([]string{
		"decl",
		pkgPath,
		kind,
		qualified,
		filepath.ToSlash(rangeInfo.File) + ":" + strconv.Itoa(rangeInfo.StartLine) + ":" + strconv.Itoa(rangeInfo.StartColumn),
	}, ":")
}

func importNodeID(file string, rangeInfo SourceRange, importPath string) string {
	return strings.Join([]string{
		"import",
		filepath.ToSlash(file),
		strconv.Itoa(rangeInfo.StartLine),
		strconv.Itoa(rangeInfo.StartColumn),
		importPath,
	}, ":")
}

func referenceNodeID(file string, rangeInfo SourceRange, name string) string {
	return strings.Join([]string{
		"reference",
		filepath.ToSlash(file),
		strconv.Itoa(rangeInfo.StartLine),
		strconv.Itoa(rangeInfo.StartColumn),
		name,
	}, ":")
}

func declarationNodeKind(id string) string {
	if strings.HasPrefix(id, "external:") {
		return "external_declaration"
	}

	return "declaration"
}

func packageNodeID(packageID string) codegraph.NodeID {
	return codegraph.NodeID("package:" + packageID)
}

func packageAPINodeID(packageID string) codegraph.NodeID {
	return codegraph.NodeID("api:" + packageID)
}

func fileNodeID(path string) codegraph.NodeID {
	return codegraph.NodeID("file:" + filepath.ToSlash(path))
}

func graphProvenance(provenance Provenance) codegraph.Provenance {
	return codegraph.Provenance{
		Source:       provenance.Source,
		File:         provenance.Range.File,
		StartLine:    provenance.Range.StartLine,
		StartColumn:  provenance.Range.StartColumn,
		EndLine:      provenance.Range.EndLine,
		EndColumn:    provenance.Range.EndColumn,
		BuildContext: provenance.Build.String(),
		Confidence:   provenance.Confidence,
	}
}

func tokenKind(tok token.Token) string {
	switch tok {
	case token.CONST:
		return "const"
	case token.VAR:
		return "var"
	default:
		return strings.ToLower(tok.String())
	}
}

func callObject(info *types.Info, call *ast.CallExpr) types.Object {
	switch fun := call.Fun.(type) {
	case *ast.Ident:
		return info.Uses[fun]
	case *ast.SelectorExpr:
		return info.Uses[fun.Sel]
	default:
		return nil
	}
}

func objectKind(object types.Object) string {
	switch object := object.(type) {
	case *types.Func:
		if signature, ok := object.Type().(*types.Signature); ok && signature.Recv() != nil {
			return kindMethod
		}
		return kindFunc
	case *types.TypeName:
		return "type"
	case *types.Const:
		return "const"
	case *types.Var:
		return "var"
	case *types.PkgName:
		return "package"
	case *types.Builtin:
		return kindBuiltin
	case *types.Label:
		return "label"
	default:
		return "identifier"
	}
}

func objectKey(object types.Object) string {
	if object == nil {
		return ""
	}

	pkgPath := "builtin"
	if object.Pkg() != nil && object.Pkg().Path() != "" {
		pkgPath = object.Pkg().Path()
	}

	receiver := ""
	if fn, ok := object.(*types.Func); ok {
		if signature, ok := fn.Type().(*types.Signature); ok && signature.Recv() != nil {
			receiver = receiverObjectKey(signature.Recv().Type())
		}
	}

	return strings.Join([]string{pkgPath, objectKind(object), receiver, object.Id()}, "\x00")
}

func receiverObjectKey(receiver types.Type) string {
	if pointer, ok := receiver.(*types.Pointer); ok {
		receiver = pointer.Elem()
	}
	if named, ok := receiver.(*types.Named); ok {
		name := named.Obj().Name()
		if named.Obj().Pkg() != nil && named.Obj().Pkg().Path() != "" {
			return named.Obj().Pkg().Path() + "." + name
		}

		return name
	}

	return types.TypeString(receiver, func(pkg *types.Package) string {
		return pkg.Path()
	})
}

func referenceConfidence(targetID string) string {
	if strings.HasPrefix(targetID, "external:") {
		return "medium"
	}

	return "high"
}

func declarationLess(left, right Declaration) bool {
	if left.Name != right.Name {
		return left.Name < right.Name
	}
	if left.File != right.File {
		return left.File < right.File
	}
	if left.Range.StartLine != right.Range.StartLine {
		return left.Range.StartLine < right.Range.StartLine
	}

	return left.ID < right.ID
}

func importLess(left, right Import) bool {
	if left.Path != right.Path {
		return left.Path < right.Path
	}
	if left.File != right.File {
		return left.File < right.File
	}
	if left.Range.StartLine != right.Range.StartLine {
		return left.Range.StartLine < right.Range.StartLine
	}

	return left.ID < right.ID
}

func referenceLess(left, right Reference) bool {
	if left.Name != right.Name {
		return left.Name < right.Name
	}
	if left.File != right.File {
		return left.File < right.File
	}
	if left.Range.StartLine != right.Range.StartLine {
		return left.Range.StartLine < right.Range.StartLine
	}
	if left.Range.StartColumn != right.Range.StartColumn {
		return left.Range.StartColumn < right.Range.StartColumn
	}

	return left.ID < right.ID
}

func callEdgeLess(left, right CallEdge) bool {
	if left.CallerID != right.CallerID {
		return left.CallerID < right.CallerID
	}
	if left.CalleeID != right.CalleeID {
		return left.CalleeID < right.CalleeID
	}
	if left.File != right.File {
		return left.File < right.File
	}
	if left.Range.StartLine != right.Range.StartLine {
		return left.Range.StartLine < right.Range.StartLine
	}

	return left.Range.StartColumn < right.Range.StartColumn
}
