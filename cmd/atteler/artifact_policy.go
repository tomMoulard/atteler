package main

import (
	"fmt"
	"path/filepath"
	"strings"
)

func attelerArtifactPrivacyHint(path string) (string, bool) {
	clean := filepath.ToSlash(filepath.Clean(strings.TrimSpace(path)))

	if clean == "." || clean == "" {
		return "", false
	}

	if clean == ".atteler" || strings.HasPrefix(clean, ".atteler/") || strings.Contains(clean, "/.atteler/") {
		return fmt.Sprintf("privacy_hint=%s is ignored/private by default; review and redact before copying to a committed .atteler asset path", path), true
	}

	return "", false
}

func formatAttelerArtifactPrivacyHint(path string) string {
	hint, ok := attelerArtifactPrivacyHint(path)
	if !ok {
		return ""
	}

	return hint + "\n"
}

func printAttelerArtifactPrivacyHint(path string) {
	fmt.Print(formatAttelerArtifactPrivacyHint(path))
}
