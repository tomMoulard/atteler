package lsp

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/tommoulard/atteler/pkg/permission"
)

var defaultPool = NewServerPool(PoolOptions{})

// DocumentSymbols opens Options.FilePath on a managed language-server session,
// requests textDocument/documentSymbol, and normalizes the result. Calls with
// the same root, language, command, args, and environment reuse a healthy
// server from the default pool unless Options.Pool is set.
func DocumentSymbols(ctx context.Context, opts Options) ([]Symbol, error) {
	if err := requireContext(ctx, "symbols"); err != nil {
		return nil, err
	}

	return poolForOptions(opts).DocumentSymbols(ctx, opts)
}

// WorkspaceSymbols requests workspace/symbol on a managed language-server
// session and normalizes SymbolInformation results. Calls with the same root,
// language, command, args, and environment reuse a healthy server from the
// default pool unless Options.Pool is set.
func WorkspaceSymbols(ctx context.Context, opts Options, query string) ([]Symbol, error) {
	if err := requireContext(ctx, "workspace symbols"); err != nil {
		return nil, err
	}

	return poolForOptions(opts).WorkspaceSymbols(ctx, opts, query)
}

// Definitions requests textDocument/definition for Options.FilePath at pos on
// a managed language-server session.
func Definitions(ctx context.Context, opts Options, pos Position) ([]Location, error) {
	if ctx == nil {
		return nil, errors.New("lsp definitions: nil context")
	}

	return poolForOptions(opts).Definitions(ctx, opts, pos)
}

// References requests textDocument/references for Options.FilePath at pos on a
// managed language-server session.
func References(ctx context.Context, opts Options, pos Position, includeDeclaration bool) ([]Location, error) {
	if ctx == nil {
		return nil, errors.New("lsp references: nil context")
	}

	return poolForOptions(opts).References(ctx, opts, pos, includeDeclaration)
}

// ShutdownDefaultPool gracefully shuts down sessions created by the package
// convenience functions when Options.Pool was not set.
func ShutdownDefaultPool(ctx context.Context) error {
	return defaultPool.Shutdown(ctx)
}

func poolForOptions(opts Options) *ServerPool {
	if opts.Pool != nil {
		return opts.Pool
	}

	return defaultPool
}

func requireContext(ctx context.Context, scope string) error {
	if ctx == nil {
		return fmt.Errorf("lsp %s: context is required", scope)
	}

	if err := ctx.Err(); err != nil {
		return fmt.Errorf("lsp %s: context already done: %w", scope, err)
	}

	return nil
}

func validateOptions(opts Options) error {
	if err := validateWorkspaceOptions(opts); err != nil {
		return err
	}

	if strings.TrimSpace(opts.FilePath) == "" {
		return errors.New("file path is required")
	}

	return nil
}

func validateWorkspaceOptions(opts Options) error {
	if strings.TrimSpace(opts.Command) == "" {
		return errors.New("lsp command is required")
	}

	return nil
}

type documentRequest struct {
	RootPath   string
	LanguageID string
	URI        string
	Content    string
}

func resolveDocumentRequest(ctx context.Context, opts Options) (documentRequest, error) {
	if err := validateOptions(opts); err != nil {
		return documentRequest{}, err
	}

	filePath, err := filepath.Abs(strings.TrimSpace(opts.FilePath))
	if err != nil {
		return documentRequest{}, fmt.Errorf("resolve file path: %w", err)
	}

	if policyErr := authorizeLSPReadPermission(ctx, "read LSP document", filePath); policyErr != nil {
		return documentRequest{}, policyErr
	}

	content, err := os.ReadFile(filePath)
	if err != nil {
		return documentRequest{}, fmt.Errorf("read file %q: %w", filePath, err)
	}

	rootPath := strings.TrimSpace(opts.RootPath)
	if rootPath == "" {
		rootPath = filepath.Dir(filePath)
	}

	rootPath, err = filepath.Abs(rootPath)
	if err != nil {
		return documentRequest{}, fmt.Errorf("resolve root path: %w", err)
	}

	languageID := strings.TrimSpace(opts.LanguageID)
	if languageID == "" {
		languageID = inferLanguageID(filePath)
	}

	return documentRequest{
		RootPath:   rootPath,
		LanguageID: languageID,
		URI:        fileURI(filePath),
		Content:    string(content),
	}, nil
}

func resolveWorkspaceRequest(ctx context.Context, opts Options) (rootPath, languageID string, err error) {
	rootPath, languageID, err = workspaceRequestParts(opts)
	if err != nil {
		return "", "", err
	}

	if policyErr := authorizeLSPReadPermission(ctx, "read LSP workspace", rootPath); policyErr != nil {
		return "", "", policyErr
	}

	return rootPath, languageID, nil
}

func workspaceRequestParts(opts Options) (rootPath, languageID string, err error) {
	if validationErr := validateWorkspaceOptions(opts); validationErr != nil {
		return "", "", validationErr
	}

	rootPath = strings.TrimSpace(opts.RootPath)
	if rootPath == "" {
		rootPath, err = os.Getwd()
		if err != nil {
			return "", "", fmt.Errorf("resolve current directory: %w", err)
		}
	}

	rootPath, err = filepath.Abs(rootPath)
	if err != nil {
		return "", "", fmt.Errorf("resolve root path: %w", err)
	}

	return rootPath, strings.TrimSpace(opts.LanguageID), nil
}

func authorizeLSPReadPermission(ctx context.Context, action, target string) error {
	decision := permission.Evaluate(ctx, nil, permission.Request{
		Action: action,
		Source: "atteler.lsp",
		Target: target,
		Operations: []permission.Operation{{
			Kind:   permission.OperationRead,
			Action: action,
			Source: "atteler.lsp",
			Target: target,
		}},
	})
	if decision.Allowed {
		return nil
	}

	return &permission.Error{Decision: decision}
}
