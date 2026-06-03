//nolint:wsl_v5 // Persisted DTO field order mirrors the on-disk snapshot shape.
package codeintel

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/tommoulard/atteler/pkg/codegraph"
)

const persistedWorkspaceIndexVersion = 2

type persistedWorkspaceIndex struct {
	Fingerprints map[string]fileFingerprint    `json:"fingerprints"`
	FileModels   map[string]persistedFileModel `json:"file_models,omitempty"`
	Root         string                        `json:"root"`
	OptionsKey   string                        `json:"options_key"`
	Fingerprint  string                        `json:"fingerprint"`
	Index        persistedIndex                `json:"index"`
	Version      int                           `json:"version"`
}

type persistedFileModel struct {
	Language    string          `json:"language"`
	Fingerprint fileFingerprint `json:"fingerprint"`
	Model       Model           `json:"model"`
}

type persistedIndex struct {
	Imports            []Import                 `json:"imports"`
	GraphNodes         []codegraph.Node         `json:"graph_nodes"`
	ImportEdges        []ImportEdge             `json:"import_edges"`
	Packages           []PackageInfo            `json:"packages"`
	FileDetails        []SourceFile             `json:"file_details"`
	Declarations       []Declaration            `json:"declarations"`
	CallEdges          []CallEdge               `json:"call_edges"`
	Files              []File                   `json:"files"`
	Symbols            []Symbol                 `json:"symbols"`
	Diagnostics        []Diagnostic             `json:"diagnostics"`
	GraphRelationships []codegraph.Relationship `json:"graph_relationships"`
	References         []Reference              `json:"references"`
	Model              Model                    `json:"model"`
	Stats              IndexStats               `json:"stats"`
}

func readPersistedWorkspaceIndex(path string) (persistedWorkspaceIndex, bool, error) {
	if path == "" {
		return persistedWorkspaceIndex{}, false, nil
	}

	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return persistedWorkspaceIndex{}, false, nil
	}
	if err != nil {
		return persistedWorkspaceIndex{}, false, fmt.Errorf("read code-intelligence index %s: %w", path, err)
	}

	var snapshot persistedWorkspaceIndex
	//nolint:musttag // Snapshot DTOs wrap existing model structs that intentionally keep their public field names on disk.
	if err := json.Unmarshal(data, &snapshot); err != nil {
		return persistedWorkspaceIndex{}, false, fmt.Errorf("decode code-intelligence index %s: %w", path, err)
	}
	if snapshot.Version != persistedWorkspaceIndexVersion {
		return persistedWorkspaceIndex{}, false, nil
	}

	return snapshot, true, nil
}

func writePersistedWorkspaceIndex(path string, snapshot persistedWorkspaceIndex) error {
	if path == "" {
		return nil
	}

	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return fmt.Errorf("create code-intelligence index directory: %w", err)
	}

	//nolint:musttag // Snapshot DTOs wrap existing model structs that intentionally keep their public field names on disk.
	data, err := json.MarshalIndent(snapshot, "", "  ")
	if err != nil {
		return fmt.Errorf("encode code-intelligence index: %w", err)
	}

	tmp, err := os.CreateTemp(filepath.Dir(path), ".codeintel-*.json")
	if err != nil {
		return fmt.Errorf("create temporary code-intelligence index: %w", err)
	}
	tmpPath := tmp.Name()
	defer func() { _ = os.Remove(tmpPath) }()

	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write temporary code-intelligence index: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temporary code-intelligence index: %w", err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return fmt.Errorf("replace code-intelligence index: %w", err)
	}

	return nil
}

func newPersistedWorkspaceIndex(
	root string,
	optsKey string,
	fingerprint string,
	fingerprints map[string]fileFingerprint,
	fileModels map[string]persistedFileModel,
	index Index,
) persistedWorkspaceIndex {
	return persistedWorkspaceIndex{
		Version:      persistedWorkspaceIndexVersion,
		Root:         cleanPath(root),
		OptionsKey:   optsKey,
		Fingerprint:  fingerprint,
		Fingerprints: cloneFileFingerprints(fingerprints),
		FileModels:   clonePersistedFileModels(fileModels),
		Index:        snapshotIndex(index),
	}
}

func clonePersistedWorkspaceIndex(snapshot persistedWorkspaceIndex) persistedWorkspaceIndex {
	return persistedWorkspaceIndex{
		Version:      snapshot.Version,
		Root:         snapshot.Root,
		OptionsKey:   snapshot.OptionsKey,
		Fingerprint:  snapshot.Fingerprint,
		Fingerprints: cloneFileFingerprints(snapshot.Fingerprints),
		FileModels:   clonePersistedFileModels(snapshot.FileModels),
		Index:        snapshotIndex(restoreIndex(snapshot.Index)),
	}
}

func snapshotIndex(index Index) persistedIndex {
	snapshot := persistedIndex{
		Files:        cloneFiles(index.Files),
		Symbols:      append([]Symbol(nil), index.Symbols...),
		ImportEdges:  append([]ImportEdge(nil), index.ImportEdges...),
		Packages:     clonePackages(index.Packages),
		FileDetails:  cloneSourceFiles(index.FileDetails),
		Declarations: cloneDeclarations(index.Declarations),
		Imports:      cloneImports(index.Imports),
		References:   cloneReferences(index.References),
		CallEdges:    cloneCallEdges(index.CallEdges),
		Diagnostics:  append([]Diagnostic(nil), index.Diagnostics...),
		Model:        cloneModel(index.Model),
		Stats:        index.Stats,
	}
	if index.Graph != nil {
		snapshot.GraphNodes = append([]codegraph.Node(nil), index.Graph.Nodes()...)
		snapshot.GraphRelationships = cloneGraphRelationships(index.Graph.Relationships())
	}

	return snapshot
}

func restoreIndex(snapshot persistedIndex) Index {
	index := Index{
		Files:       cloneFiles(snapshot.Files),
		Symbols:     append([]Symbol(nil), snapshot.Symbols...),
		ImportEdges: append([]ImportEdge(nil), snapshot.ImportEdges...),

		Packages:     clonePackages(snapshot.Packages),
		FileDetails:  cloneSourceFiles(snapshot.FileDetails),
		Declarations: cloneDeclarations(snapshot.Declarations),
		Imports:      cloneImports(snapshot.Imports),
		References:   cloneReferences(snapshot.References),
		CallEdges:    cloneCallEdges(snapshot.CallEdges),
		Graph:        codegraph.NewEvidence(),
		Diagnostics:  append([]Diagnostic(nil), snapshot.Diagnostics...),
		Model:        cloneModel(snapshot.Model),
		Stats:        snapshot.Stats,
	}

	for i := range snapshot.GraphNodes {
		index.Graph.AddNode(snapshot.GraphNodes[i])
	}
	for i := range snapshot.GraphRelationships {
		index.Graph.AddRelationship(snapshot.GraphRelationships[i])
	}

	return index
}

func cloneGraphRelationships(relationships []codegraph.Relationship) []codegraph.Relationship {
	out := append([]codegraph.Relationship(nil), relationships...)
	for i := range out {
		out[i].Provenance = append([]codegraph.Provenance(nil), out[i].Provenance...)
	}

	return out
}

func persistedIndexMatches(snapshot persistedWorkspaceIndex, root, optsKey, fingerprint string) bool {
	return snapshot.Root == cleanPath(root) && snapshot.OptionsKey == optsKey && snapshot.Fingerprint == fingerprint
}

func clonePersistedFileModels(models map[string]persistedFileModel) map[string]persistedFileModel {
	if len(models) == 0 {
		return nil
	}

	out := make(map[string]persistedFileModel, len(models))
	for path := range models {
		model := models[path]
		model.Fingerprint = cloneFileFingerprint(model.Fingerprint)
		model.Model = cloneModel(model.Model)
		out[path] = model
	}

	return out
}

func cloneFileFingerprint(fingerprint fileFingerprint) fileFingerprint {
	return fileFingerprint{
		Path:    fingerprint.Path,
		Hash:    fingerprint.Hash,
		ModTime: fingerprint.ModTime,
		Size:    fingerprint.Size,
	}
}
