// Package main demonstrates session persistence with optional worktree attachment.
package main

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"time"

	"github.com/tommoulard/atteler/pkg/sdk"
	"github.com/tommoulard/atteler/pkg/session"
)

func main() {
	store := session.NewStore(filepath.Join(os.TempDir(), "atteler-session-example"))
	sessionState := sdk.NewSession(sdk.SessionOptions{
		Model: "fake-model",
		Title: "SDK worktree/session example",
		Tags:  []string{"sdk", "example"},
	})

	if repo := os.Getenv("ATTELER_EXAMPLE_REPO"); repo != "" {
		if _, err := sdk.AttachNewWorktree(exampleContext{}, repo, &sessionState); err != nil {
			log.Fatal(err)
		}
	}

	if err := sdk.SaveSession(store, sessionState); err != nil {
		log.Fatal(err)
	}

	fmt.Printf("session=%s path=%s worktree=%s\n", sessionState.ID, store.Path(sessionState.ID), sessionState.WorktreePath)
}

// exampleContext keeps examples free of process-root context creation; real applications should pass their request or command context.
type exampleContext struct{}

func (exampleContext) Deadline() (time.Time, bool) { return time.Time{}, false }

func (exampleContext) Done() <-chan struct{} { return nil }

func (exampleContext) Err() error { return nil }

func (exampleContext) Value(_ any) any { return nil }
