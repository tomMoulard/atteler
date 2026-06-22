package sdk_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/tommoulard/atteler/pkg/llm"
	"github.com/tommoulard/atteler/pkg/memory"
	"github.com/tommoulard/atteler/pkg/plugin"
	"github.com/tommoulard/atteler/pkg/review"
	"github.com/tommoulard/atteler/pkg/sdk"
	"github.com/tommoulard/atteler/pkg/session"
	"github.com/tommoulard/atteler/pkg/worktree"
)

func ExampleRunOneShotChat() {
	registry, err := sdk.NewProviderRegistry(fakeProvider{name: "fake", models: []string{"fake-model"}})
	if err != nil {
		panic(err)
	}

	result, err := sdk.RunOneShotChat(context.Background(), sdk.OneShotChatOptions{
		Registry: registry,
		Model:    "fake-model",
		Prompt:   "hello SDK",
	})
	if err != nil {
		panic(err)
	}

	fmt.Println(result.Response.Content)

	// Output:
	// echo: hello SDK
}

func ExampleNewProviderRegistry() {
	registry, err := sdk.NewProviderRegistry(fakeProvider{name: "fake", models: []string{"fake-model"}})
	if err != nil {
		panic(err)
	}

	provider, model, ok := registry.ResolveModel("fake-model")
	fmt.Println(provider, model, ok)

	// Output:
	// fake fake-model true
}

func ExamplePackageContracts() {
	contracts := sdk.PackageContracts()

	fmt.Println(contracts[0].ImportPath, contracts[0].Stability)

	// Output:
	// github.com/tommoulard/atteler/pkg/sdk stable
}

func ExampleAPIContract() {
	contract := sdk.APIContract()

	fmt.Println(contract.SchemaVersion, len(contract.Packages) > 0)

	// Output:
	// atteler.sdk.v1 true
}

func ExampleBuildMemoryIndex() {
	store, err := sdk.BuildMemoryIndex(sdk.MemoryIndexOptions{
		Documents: []memory.Document{{
			ID:   "session-1",
			Text: "Review runs can index memory and retrieve prior decisions.",
		}},
	})
	if err != nil {
		panic(err)
	}

	results, err := sdk.SearchMemory(store, "retrieve decisions", 1)
	if err != nil {
		panic(err)
	}

	fmt.Println(results[0].Document.ID)

	// Output:
	// session-1
}

func ExampleNewReviewRun() {
	run, err := sdk.NewReviewRun(sdk.ReviewRunOptions{
		Reviewers: []review.Reviewer{{Name: "quality-reviewer"}},
		Paths:     []string{"pkg/sdk"},
	})
	if err != nil {
		panic(err)
	}

	rounds := run.Plan.Rounds()
	for i := range rounds {
		round := rounds[i]
		fmt.Println(round.Number, round.Kind)
	}

	// Output:
	// 1 independent-review
	// 2 cross-review
	// 3 aggregate-verdict
}

func ExampleRunPlugin() {
	root, err := os.MkdirTemp("", "atteler-sdk-plugin-*")
	if err != nil {
		panic(err)
	}

	defer func() {
		if cleanupErr := os.RemoveAll(root); cleanupErr != nil {
			panic(cleanupErr)
		}
	}()

	writeExampleScript(filepath.Join(root, "bin", "hello"), "#!/bin/sh\nprintf 'plugin: %s\\n' \"$1\"\n")

	manifest := plugin.Manifest{
		Name:                  "hello",
		Version:               "1.0.0",
		MinimumAttelerVersion: "0.1.0",
		Entrypoints:           map[string]string{"run": "bin/hello"},
		EntrypointContracts: map[string]plugin.EntrypointContract{
			"run": {
				Inputs: plugin.EntrypointInputs{Args: []plugin.ArgumentSpec{{Name: "subject", Required: true}}},
				Output: &plugin.StructuredOutputContract{Format: plugin.OutputFormatText},
			},
		},
		Permissions: &plugin.PermissionSet{
			Filesystem: plugin.FilesystemPermissions{Read: []string{"."}},
			Shell:      plugin.ShellPermissions{Allow: true},
		},
		Output: &plugin.OutputLimits{StdoutMaxBytes: 4096, StderrMaxBytes: 4096},
		Trust: &plugin.Trust{
			Enabled:       true,
			InstallSource: "example",
			Checksum:      "sha256:example",
			Audit:         []plugin.TrustAudit{{Action: "accepted", Actor: "example", At: "2026-06-22T00:00:00Z"}},
		},
	}
	policy := plugin.AcceptManifestPolicy(manifest)

	result, err := sdk.RunPlugin(context.Background(), sdk.PluginRunOptions{
		Policy:     &policy,
		Manifest:   manifest,
		Root:       root,
		Entrypoint: "run",
		Args:       []string{"SDK"},
		Timeout:    5 * time.Second,
	})
	if err != nil {
		panic(err)
	}

	fmt.Print(result.Stdout)

	// Output:
	// plugin: SDK
}

func ExampleNewSession() {
	store := session.NewStore(filepath.Join(os.TempDir(), "atteler-sdk-session-example"))
	sessionState := sdk.NewSession(sdk.SessionOptions{
		Model: "fake-model",
		Messages: []llm.Message{{
			Role:    llm.RoleUser,
			Content: "remember this",
		}},
		Tags: []string{"example"},
	})

	fmt.Println(sessionState.DefaultModel, len(sessionState.Messages), store.Path(sessionState.ID) != "")

	// Output:
	// fake-model 1 true
}

func ExampleAttachWorktree() {
	sessionState := sdk.NewSession(sdk.SessionOptions{Model: "fake-model"})
	sdk.AttachWorktree(&sessionState, &worktree.Info{
		Path:       "/repo/.atteler/worktrees/session-1",
		Branch:     "atteler/session-1",
		BaseBranch: "main",
		SessionID:  sessionState.ID,
	})

	fmt.Println(sessionState.WorktreeBranch, sessionState.WorktreeBase)

	// Output:
	// atteler/session-1 main
}

func writeExampleScript(path, content string) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		panic(err)
	}

	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		panic(err)
	}

	//nolint:gosec // SDK example intentionally creates a local executable plugin script.
	if err := os.Chmod(path, 0o700); err != nil {
		panic(err)
	}
}
