package main

import (
	"path/filepath"
	"strings"

	"github.com/tommoulard/atteler/pkg/codeintel"
)

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
