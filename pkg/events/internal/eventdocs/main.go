// Package main renders generated lifecycle hook documentation.
package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"github.com/tommoulard/atteler/pkg/events"
)

func main() {
	outPath := flag.String("out", "", "output markdown path")

	flag.Parse()

	if *outPath == "" {
		fmt.Fprintln(os.Stderr, "eventdocs: -out is required")
		os.Exit(2)
	}

	if err := os.MkdirAll(filepath.Dir(*outPath), 0o750); err != nil {
		fmt.Fprintf(os.Stderr, "eventdocs: create output dir: %v\n", err)
		os.Exit(1)
	}

	if err := os.WriteFile(*outPath, []byte(events.EventDocsMarkdown()), 0o600); err != nil {
		fmt.Fprintf(os.Stderr, "eventdocs: write output: %v\n", err)
		os.Exit(1)
	}
}
