package main

import (
	"fmt"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/tommoulard/atteler/pkg/codeintel"
)

func showCodeFile(root, target string) error {
	idx, err := codeintel.IndexDir(root)
	if err != nil {
		return fmt.Errorf("code file: index %s: %w", root, err)
	}

	file, ok := findCodeFile(root, idx, target)
	if !ok {
		fmt.Println("No Go code file found.")
		return nil
	}

	printCodeFile(root, file)

	return nil
}

func findCodeFile(root string, idx codeintel.Index, target string) (codeintel.File, bool) {
	target = filepath.ToSlash(strings.TrimSpace(target))

	for i := range idx.Files {
		rel := relativeCodePath(root, idx.Files[i].Path)

		abs := filepath.ToSlash(idx.Files[i].Path)
		if rel == target || abs == target {
			return idx.Files[i], true
		}
	}

	return codeintel.File{}, false
}

func printCodeFile(root string, file codeintel.File) {
	fmt.Println(formatCodeFile(root, file))

	if len(file.Imports) > 0 {
		fmt.Println("imports:")

		for _, imp := range file.Imports {
			fmt.Println("  - " + imp)
		}
	}

	if len(file.Symbols) > 0 {
		fmt.Println("symbols:")

		for i := range file.Symbols {
			fmt.Println("  - " + formatCodeFileSymbol(file.Symbols[i]))
		}
	}
}

func formatCodeFile(root string, file codeintel.File) string {
	return "path=" + relativeCodePath(root, file.Path) + "	package=" + file.Package + "	imports=" + strconv.Itoa(len(file.Imports)) + "	symbols=" + strconv.Itoa(len(file.Symbols))
}

func formatCodeFileSymbol(symbol codeintel.Symbol) string {
	return symbol.Name + "	kind=" + symbol.Kind + "	line=" + strconv.Itoa(symbol.Line)
}
