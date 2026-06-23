// Package main demonstrates indexing and searching local SDK memory.
package main

import (
	"fmt"
	"log"

	"github.com/tommoulard/atteler/pkg/memory"
	"github.com/tommoulard/atteler/pkg/sdk"
)

func main() {
	store, err := sdk.BuildMemoryIndex(sdk.MemoryIndexOptions{
		Documents: []memory.Document{
			{ID: "session-1", Text: "The provider registry owns model resolution."},
			{ID: "session-2", Text: "Memory search returns ranked snippets."},
		},
	})
	if err != nil {
		log.Fatal(err)
	}

	results, err := sdk.SearchMemory(store, "model registry", 1)
	if err != nil {
		log.Fatal(err)
	}

	fmt.Printf("%s: %s\n", results[0].Document.ID, results[0].Snippet)
}
