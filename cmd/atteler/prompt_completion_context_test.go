package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tommoulard/atteler/pkg/agent"
	"github.com/tommoulard/atteler/pkg/autonomy"
	"github.com/tommoulard/atteler/pkg/codeintel"
	"github.com/tommoulard/atteler/pkg/config"
	"github.com/tommoulard/atteler/pkg/contextref"
	"github.com/tommoulard/atteler/pkg/llm"
	"github.com/tommoulard/atteler/pkg/promptcomplete"
	"github.com/tommoulard/atteler/pkg/session"
	"github.com/tommoulard/atteler/pkg/shell"
	"github.com/tommoulard/atteler/pkg/tasklist"
	"github.com/tommoulard/atteler/pkg/worktree"
)

const (
	promptContextTestReturnBudget     = 750 * time.Millisecond
	promptContextTestBackgroundBudget = 5 * time.Second
	promptContextTestGitStatusArgs    = "status --short"
	promptContextTestGitBranchArgs    = "branch --show-current"
)

//nolint:paralleltest // Uses process-wide prompt context source hooks.
func TestPromptCompletionContext_UsesRepoSessionAndTaskSources(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "live.go"), []byte("package live\n\nfunc LiveSymbol() {}\n"), 0o600))

	restoreIndex := setPromptCodeIndexDirContextForTest(func(_ context.Context, root string) (codeintel.Index, error) {
		return codeintel.Index{Symbols: []codeintel.Symbol{{
			Name: "LiveSymbol",
			Kind: "func",
			File: filepath.Join(root, "live.go"),
			Line: 3,
		}}}, nil
	})
	t.Cleanup(restoreIndex)

	store := session.NewStore(filepath.Join(dir, ".atteler", "sessions"))
	taskStore := tasklist.NewStore(taskListPath(store, ""))
	_, err := taskStore.Add(context.Background(), tasklist.AddRequest{
		ID:    "GH-27",
		Title: "Make prompt completion context-aware",
	})
	require.NoError(t, err)

	state := appState{
		cwd:          dir,
		sessionStore: store,
		sessionState: session.Session{
			Title: "Prompt completion work for GH-27 and #15",
			Artifacts: []session.Artifact{{
				Path:    "docs/notes.md",
				Kind:    "notes",
				Summary: "context source notes",
			}},
		},
	}

	completionContext := promptCompletionContext(context.Background(), state, "fix symbol Live", true)
	suggestions := promptcomplete.SuggestAll(completionContext, promptcomplete.Options{})

	require.NotEmpty(t, suggestions)
	assert.Equal(t, "LiveSymbol", suggestions[0].Text)
	assert.Equal(t, "project-symbol", suggestions[0].Candidate.Kind)
	assert.NotEmpty(t, completionContext.Tasks)
	assert.Contains(t, candidateTexts(completionContext.Tasks), "GH-27")
	assert.Contains(t, candidateTexts(completionContext.Issues), "#15")
	assert.Contains(t, candidateTexts(completionContext.Issues), "GH-27")
	assert.Equal(t, 1, countCandidateText(completionContext.Issues, "GH-27"))
	assert.Contains(t, candidateTexts(completionContext.RecentFiles), "docs/notes.md")
}

func TestParseGitStatusPath_RenameUsesNewPath(t *testing.T) {
	t.Parallel()

	assert.Equal(t, "new/name.go", parseGitStatusPath("R  old/name.go -> new/name.go"))
	assert.Equal(t, "pkg/file.go", parseGitStatusPath(" M pkg/file.go"))
	assert.Empty(t, parseGitStatusPath(""))
}

func TestPromptGitAuditContextIncludesAutonomy(t *testing.T) {
	t.Parallel()

	got := promptGitAuditContext(autonomy.Full)
	assert.Equal(t, "atteler.prompt_completion.git", got.Caller)
	assert.Equal(t, "full", got.Autonomy)
}

func TestPromptCompletionContext_LowAutonomySkipsGitShellAudit(t *testing.T) {
	auditDir := filepath.Join(t.TempDir(), "audit")
	t.Setenv(shell.EnvAuditDir, auditDir)

	completionContext := promptCompletionContext(context.Background(), appState{
		autonomy: autonomy.Low,
		cwd:      t.TempDir(),
	}, "GH-", true)

	assert.Empty(t, completionContext.Issues)
	assert.Empty(t, completionContext.RecentFiles)
	assert.NoDirExists(t, auditDir)
}

func TestPromptLimitedOutputBuffer_LimitsBytes(t *testing.T) {
	t.Parallel()

	buffer := newPromptLimitedOutputBuffer(5)

	n, err := buffer.Write([]byte("abcdef"))

	require.NoError(t, err)
	assert.Equal(t, 6, n)
	assert.Equal(t, "abcde", buffer.String())
	assert.True(t, buffer.Truncated())

	n, err = buffer.Write([]byte("gh"))

	require.NoError(t, err)
	assert.Equal(t, 2, n)
	assert.Equal(t, "abcde", buffer.String())
	assert.True(t, buffer.Truncated())
}

//nolint:paralleltest // Uses process-wide prompt context source hooks.
func TestPromptGitRecentFileCandidates_ReturnsPartialCandidatesWhenOutputLimited(t *testing.T) {
	dir := t.TempDir()

	t.Cleanup(setPromptGitOutputFuncForTest(func(context.Context, string, ...string) (string, error) {
		return " M a.go\n M b.go\n M partial", promptContextPartialError{
			reason:          "git output exceeded 10 bytes per stream",
			outputTruncated: true,
		}
	}))

	candidates, err := promptGitRecentFileCandidatesWithError(context.Background(), dir, 5)

	var partialErr promptContextPartialError
	require.ErrorAs(t, err, &partialErr)
	assert.Equal(t, "git output exceeded 10 bytes per stream", partialErr.reason)
	assert.Equal(t, []string{"a.go", "b.go"}, candidateTexts(candidates))
}

//nolint:paralleltest // Uses process-wide prompt context source hooks.
func TestPromptGitIssueCandidates_DropsIncompleteTruncatedBranchLine(t *testing.T) {
	dir := t.TempDir()

	t.Cleanup(setPromptGitOutputFuncForTest(func(context.Context, string, ...string) (string, error) {
		return "feature/GH-12", promptContextPartialError{
			reason:          "git output exceeded 10 bytes per stream",
			outputTruncated: true,
		}
	}))

	candidates, err := promptGitIssueCandidatesWithError(context.Background(), dir, 5)

	var partialErr promptContextPartialError
	require.ErrorAs(t, err, &partialErr)
	assert.Empty(t, candidates)
}

//nolint:paralleltest // Uses process-wide prompt context source hooks.
func TestPromptCompletionContext_PreservesPartialGitCandidates(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.Mkdir(filepath.Join(dir, ".git"), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(dir, ".git", "HEAD"), []byte("ref: refs/heads/main\n"), 0o600))

	t.Cleanup(setPromptGitOutputFuncForTest(func(_ context.Context, _ string, args ...string) (string, error) {
		switch strings.Join(args, " ") {
		case promptContextTestGitStatusArgs:
			return " M kept.go\n M incomplete", promptContextPartialError{
				reason:          "git status output limited",
				outputTruncated: true,
			}
		case promptContextTestGitBranchArgs:
			return "feature/GH-129\nfeature/GH-", promptContextPartialError{
				reason:          "git branch output limited",
				outputTruncated: true,
			}
		default:
			return "", nil
		}
	}))

	result := promptCompletionContextWithOptions(
		context.Background(),
		appState{cwd: dir, promptContextCache: newPromptContextCache()},
		"kept",
		true,
		defaultPromptCompletionContextOptions(),
	)

	assert.Contains(t, candidateTexts(result.Context.RecentFiles), "kept.go")
	assert.Contains(t, candidateTexts(result.Context.Issues), "GH-129")
	assert.Equal(t, promptContextFreshnessPartial, statusForSource(t, result.Sources, promptContextSourceGitStatus).Status)
	assert.Equal(t, promptContextFreshnessPartial, statusForSource(t, result.Sources, promptContextSourceGitBranch).Status)
}

//nolint:paralleltest // Uses process-wide prompt context source hooks.
func TestPromptCompletionContext_LegacyCandidateHelpersUseBoundedSources(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "helper.go"), []byte("package helper\n\nfunc HelperSymbol() {}\n"), 0o600))

	t.Cleanup(setPromptCodeIndexDirContextForTest(func(_ context.Context, root string) (codeintel.Index, error) {
		return codeintel.Index{Symbols: []codeintel.Symbol{{
			Name: "HelperSymbol",
			Kind: "func",
			File: filepath.Join(root, "helper.go"),
			Line: 3,
		}}}, nil
	}))
	t.Cleanup(setPromptGitOutputFuncForTest(func(_ context.Context, _ string, args ...string) (string, error) {
		switch strings.Join(args, " ") {
		case promptContextTestGitStatusArgs:
			return " M helper.go\n", nil
		case promptContextTestGitBranchArgs:
			return "feature/GH-129\n", nil
		default:
			return "", nil
		}
	}))

	store := session.NewStore(filepath.Join(dir, ".atteler", "sessions"))
	taskStore := tasklist.NewStore(taskListPath(store, ""))
	_, err := taskStore.Add(context.Background(), tasklist.AddRequest{
		ID:    "GH-130",
		Title: "Keep helper wrappers bounded",
	})
	require.NoError(t, err)

	sessionState := session.Session{
		Title: "Review GH-131",
		Artifacts: []session.Artifact{{
			Path: "notes/helper.md",
			Kind: "notes",
		}},
	}

	assert.Contains(t, candidateTexts(promptProjectSymbolCandidates(context.Background(), dir, "Helper")), "HelperSymbol")
	assert.Contains(t, candidateTexts(promptGitRecentFileCandidates(context.Background(), dir)), "helper.go")
	assert.Contains(t, candidateTexts(promptGitIssueCandidates(context.Background(), dir)), "GH-129")
	assert.Contains(t, candidateTexts(promptTaskCandidates(context.Background(), store, dir)), "GH-130")
	assert.Contains(t, candidateTexts(promptIssueCandidatesFromSession(sessionState)), "GH-131")
	assert.Contains(t, candidateTexts(promptSessionFileCandidates(sessionState)), "notes/helper.md")
}

//nolint:paralleltest // Uses process-wide prompt context source hooks.
func TestPromptCompletionContext_NonPositiveCandidateLimitsUseDefault(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "default.go"), []byte("package limit\n\nfunc DefaultLimitSymbol() {}\n"), 0o600))

	t.Cleanup(setPromptCodeIndexDirContextForTest(func(_ context.Context, root string) (codeintel.Index, error) {
		return codeintel.Index{Symbols: []codeintel.Symbol{{
			Name: "DefaultLimitSymbol",
			Kind: "func",
			File: filepath.Join(root, "default.go"),
			Line: 3,
		}}}, nil
	}))
	t.Cleanup(setPromptGitOutputFuncForTest(func(_ context.Context, _ string, args ...string) (string, error) {
		switch strings.Join(args, " ") {
		case promptContextTestGitStatusArgs:
			return " M default.go\n", nil
		case promptContextTestGitBranchArgs:
			return "feature/GH-129\n", nil
		default:
			return "", nil
		}
	}))

	store := session.NewStore(filepath.Join(dir, ".atteler", "sessions"))
	taskStore := tasklist.NewStore(taskListPath(store, ""))
	_, err := taskStore.Add(context.Background(), tasklist.AddRequest{
		ID:    "GH-132",
		Title: "Default non-positive prompt context limits",
	})
	require.NoError(t, err)

	symbols, err := promptProjectSymbolCandidatesWithError(context.Background(), dir, "Default", 0, defaultPromptContextMaxIndexFiles)
	require.NoError(t, err)
	files, err := promptGitRecentFileCandidatesWithError(context.Background(), dir, 0)
	require.NoError(t, err)
	issues, err := promptGitIssueCandidatesWithError(context.Background(), dir, 0)
	require.NoError(t, err)
	tasks, err := loadPromptTaskCandidatesWithError(context.Background(), store, dir, 0)
	require.NoError(t, err)

	filtered := filterPromptProjectSymbols([]promptcomplete.Candidate{{Text: "DefaultLimitSymbol", Kind: "project-symbol"}}, "Default", 0)

	assert.Contains(t, candidateTexts(symbols), "DefaultLimitSymbol")
	assert.Contains(t, candidateTexts(files), "default.go")
	assert.Contains(t, candidateTexts(issues), "GH-129")
	assert.Contains(t, candidateTexts(tasks), "GH-132")
	assert.Contains(t, candidateTexts(filtered), "DefaultLimitSymbol")
	assert.Equal(t, maxPromptContextCandidates, promptContextCandidateLimit(0))
	assert.Equal(t, maxPromptContextCandidates, promptContextCandidateLimit(-1))
}

func TestPromptCompletionContext_ExcludesHiddenAgents(t *testing.T) {
	t.Parallel()

	registry := agent.NewRegistry(map[string]config.AgentConfig{
		"visible":  {Description: "shown to users"},
		"internal": {Description: "hidden helper", Hidden: true},
	})

	completionContext := promptCompletionContext(context.Background(), appState{agentRegistry: registry}, "ask vis", false)

	assert.Contains(t, candidateTexts(completionContext.Agents), "visible")
	assert.NotContains(t, candidateTexts(completionContext.Agents), "internal")
}

func TestPromptCompletionContext_CacheDoesNotExposeHiddenAgents(t *testing.T) {
	t.Parallel()

	registry := agent.NewRegistry(map[string]config.AgentConfig{
		"visible":  {Description: "shown to users"},
		"internal": {Description: "hidden helper", Hidden: true},
	})
	state := appState{
		agentRegistry:      registry,
		cwd:                t.TempDir(),
		promptContextCache: newPromptContextCache(),
	}

	completionContext := promptCompletionContext(context.Background(), state, "ask vis", true)

	assert.Contains(t, candidateTexts(completionContext.Agents), "visible")
	assert.NotContains(t, candidateTexts(completionContext.Agents), "internal")
}

func TestPromptCompletionContext_DoesNotExposeHiddenSelectedAgentPermissions(t *testing.T) {
	t.Parallel()

	registry := agent.NewRegistry(map[string]config.AgentConfig{
		"visible": {Description: "shown to users"},
		"internal": {
			Description:     "hidden helper",
			Hidden:          true,
			ToolPermissions: map[string]bool{"internal-tool": true},
		},
	})
	completionContext := promptCompletionContext(context.Background(), appState{
		agentRegistry: registry,
		selectedAgent: "internal",
	}, "ask internal", false)

	assert.Contains(t, candidateTexts(completionContext.Permissions), "local-only")
	assert.NotContains(t, candidateTexts(completionContext.Permissions), "internal-tool")

	for _, candidate := range completionContext.Permissions {
		assert.NotContains(t, candidate.Description, "internal")
	}
}

func TestPromptContextCacheKey_DoesNotPersistHiddenSelectedAgentName(t *testing.T) {
	t.Parallel()

	registry := agent.NewRegistry(map[string]config.AgentConfig{
		"planner": {Description: "shown to users"},
		"internal": {
			Description: "hidden helper",
			Hidden:      true,
		},
		"secret-helper": {
			Description: "another hidden helper",
			Hidden:      true,
		},
	})
	dir := t.TempDir()
	cachePath := filepath.Join(dir, ".atteler", promptContextCacheFileName)
	state := appState{
		agentRegistry: registry,
		cwd:           dir,
		selectedAgent: "internal",
	}
	key := promptContextCacheKeyForState(state)

	otherHiddenKey := promptContextCacheKeyForState(appState{
		agentRegistry: registry,
		cwd:           dir,
		selectedAgent: "secret-helper",
	})

	assert.True(t, strings.HasPrefix(key.SelectedAgent, promptContextHiddenSelectedAgentKeyPrefix))
	assert.NotEqual(t, "internal", key.SelectedAgent)
	assert.NotEqual(t, key.SelectedAgent, otherHiddenKey.SelectedAgent)

	cache := newPromptContextCache(cachePath)
	cache.store(promptRepoContextSnapshot{
		CapturedAt: time.Now(),
		Key:        key,
		Sources: []promptContextSourceStatus{
			promptContextSourceReport(promptContextSourceProjectSymbols, promptContextFreshnessSkipped, "no Go files found"),
		},
	})

	data, err := os.ReadFile(cachePath)
	require.NoError(t, err)
	assert.NotContains(t, string(data), "internal")
	assert.NotContains(t, string(data), "secret-helper")

	var saved promptContextCacheFile
	require.NoError(t, json.Unmarshal(data, &saved)) //nolint:musttag // Internal cache file mirrors production loader.
	require.Len(t, saved.Snapshots, 1)
	assert.Equal(t, key.SelectedAgent, saved.Snapshots[0].Key.SelectedAgent)
}

//nolint:paralleltest // Uses process-wide prompt context source hooks.
func TestPromptCompletionContext_IncludeRepoFalseDoesNotWarmRepoCache(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "repo.go"), []byte("package repo\n\nfunc RepoSymbol() {}\n"), 0o600))

	indexCalls := 0
	restoreIndex := setPromptCodeIndexDirContextForTest(func(_ context.Context, _ string) (codeintel.Index, error) {
		indexCalls++

		return codeintel.Index{}, nil
	})
	t.Cleanup(restoreIndex)

	result := promptCompletionContextWithOptions(context.Background(), appState{
		cwd:                dir,
		promptContextCache: newPromptContextCache(),
	}, "Repo", false, defaultPromptCompletionContextOptions())

	assert.Empty(t, result.Context.ProjectSymbols)
	assert.Equal(t, 0, indexCalls)
	assert.Equal(t, promptContextFreshnessSkipped, statusForSource(t, result.Sources, promptContextSourceProjectSymbols).Status)
	assert.Contains(t, statusForSource(t, result.Sources, promptContextSourceProjectSymbols).Detail, "repo context disabled")
}

func TestPromptContextCache_CompatibleSnapshotDoesNotCrossSelectedAgent(t *testing.T) {
	t.Parallel()

	cache := newPromptContextCache()
	cache.store(promptRepoContextSnapshot{
		CapturedAt: time.Now(),
		Key: promptContextCacheKey{
			Root:          "/repo",
			SelectedAgent: "visible",
		},
		ProjectSymbols: []promptcomplete.Candidate{{
			Text: "VisibleAgentOnlySymbol",
			Kind: "project-symbol",
		}},
	})

	_, ok := cache.compatibleSnapshot(promptContextCacheKey{
		Root:          "/repo",
		SelectedAgent: "internal",
	})

	assert.False(t, ok)
}

func TestPromptContextCache_ClonesCandidateTokens(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 5, 26, 12, 0, 0, 0, time.UTC)
	key := promptContextCacheKey{Root: "/repo", SelectedAgent: "planner"}
	tokens := []string{"original"}
	cache := newPromptContextCache()
	cache.store(promptRepoContextSnapshot{
		CapturedAt: now,
		Key:        key,
		ProjectSymbols: []promptcomplete.Candidate{{
			Text:   "CachedSymbol",
			Kind:   "project-symbol",
			Tokens: tokens,
		}},
	})

	tokens[0] = "mutated-before-read"

	first, ok := cache.freshSnapshot(key, now, time.Minute)
	require.True(t, ok)
	require.Len(t, first.ProjectSymbols, 1)
	require.Len(t, first.ProjectSymbols[0].Tokens, 1)
	assert.Equal(t, "original", first.ProjectSymbols[0].Tokens[0])

	first.ProjectSymbols[0].Tokens[0] = "mutated-after-read"

	second, ok := cache.freshSnapshot(key, now, time.Minute)
	require.True(t, ok)
	require.Len(t, second.ProjectSymbols, 1)
	require.Len(t, second.ProjectSymbols[0].Tokens, 1)
	assert.Equal(t, "original", second.ProjectSymbols[0].Tokens[0])
}

func TestPromptContextCache_IncompleteSnapshotsRespectTTLAndRemainCompatibleFallbacks(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 5, 26, 12, 0, 0, 0, time.UTC)

	for _, freshness := range []promptContextFreshness{
		promptContextFreshnessStale,
		promptContextFreshnessPartial,
		promptContextFreshnessError,
	} {
		t.Run(string(freshness), func(t *testing.T) {
			t.Parallel()

			key := promptContextCacheKey{Root: "/repo/" + string(freshness), SelectedAgent: "planner"}
			cache := newPromptContextCache()
			cache.store(promptRepoContextSnapshot{
				CapturedAt: now,
				Key:        key,
				ProjectSymbols: []promptcomplete.Candidate{{
					Text: "IncompleteCachedSymbol",
					Kind: "project-symbol",
				}},
				Sources: []promptContextSourceStatus{
					promptContextSourceReport(promptContextSourceProjectSymbols, freshness, "needs refresh"),
				},
			})

			fresh, freshOK := cache.freshSnapshot(key, now, time.Minute)
			compatible, compatibleOK := cache.compatibleSnapshot(key)
			_, expiredOK := cache.freshSnapshot(key, now.Add(2*time.Minute), time.Minute)

			require.True(t, freshOK)
			assert.Equal(t, freshness, statusForSource(t, fresh.Sources, promptContextSourceProjectSymbols).Status)
			assert.False(t, expiredOK)
			require.True(t, compatibleOK)
			assert.Contains(t, candidateTexts(compatible.ProjectSymbols), "IncompleteCachedSymbol")
		})
	}
}

func TestPromptContextCache_DurableSnapshotDoesNotCrossSelectedAgent(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	cachePath := filepath.Join(dir, ".atteler", promptContextCacheFileName)
	writer := newPromptContextCache(cachePath)
	writer.store(promptRepoContextSnapshot{
		CapturedAt: time.Now(),
		Key: promptContextCacheKey{
			Root:          dir,
			SelectedAgent: "visible",
		},
		ProjectSymbols: []promptcomplete.Candidate{{
			Text: "VisibleAgentOnlySymbol",
			Kind: "project-symbol",
		}},
	})

	reader := newPromptContextCache(cachePath)
	key := promptContextCacheKey{
		Root:          dir,
		SelectedAgent: "internal",
	}
	_, freshOK := reader.freshSnapshot(key, time.Now(), time.Minute)
	_, compatibleOK := reader.compatibleSnapshot(key)

	assert.False(t, freshOK)
	assert.False(t, compatibleOK)
}

func TestPromptContextCache_ConcurrentStoresPersistLatestSnapshotSet(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	cachePath := filepath.Join(dir, ".atteler", promptContextCacheFileName)
	cache := newPromptContextCache(cachePath)
	now := time.Date(2026, 5, 26, 12, 0, 0, 0, time.UTC)

	const snapshotCount = 8

	keys := make([]promptContextCacheKey, snapshotCount)
	for i := range snapshotCount {
		keys[i] = promptContextCacheKey{
			Root:            filepath.Join(dir, "repo-"+strconv.Itoa(i)),
			GitRevision:     "git-" + strconv.Itoa(i),
			SelectedAgent:   "planner",
			SessionRevision: "session",
			TaskRevision:    "task",
		}
	}

	var wg sync.WaitGroup

	for i, key := range keys {
		wg.Go(func() {
			cache.store(promptRepoContextSnapshot{
				CapturedAt: now.Add(time.Duration(i) * time.Second),
				Key:        key,
				ProjectSymbols: []promptcomplete.Candidate{{
					Text: "ConcurrentSymbol" + strconv.Itoa(i),
					Kind: "project-symbol",
				}},
			})
		})
	}

	wg.Wait()

	reader := newPromptContextCache(cachePath)
	for i, key := range keys {
		snapshot, ok := reader.freshSnapshot(key, now.Add(time.Minute), time.Hour)
		require.True(t, ok, "snapshot %d should survive concurrent durable saves", i)
		require.Len(t, snapshot.ProjectSymbols, 1)
		assert.Equal(t, "ConcurrentSymbol"+strconv.Itoa(i), snapshot.ProjectSymbols[0].Text)
	}
}

func TestPromptContextCache_RevisionsInvalidateFreshSnapshots(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 5, 26, 12, 0, 0, 0, time.UTC)
	baseKey := promptContextCacheKey{
		Root:            "/repo",
		GitRevision:     "git-a",
		SelectedAgent:   "planner",
		SessionRevision: "session-a",
		TaskRevision:    "task-a",
	}

	cache := newPromptContextCache()
	cache.store(promptRepoContextSnapshot{
		CapturedAt: now,
		Key:        baseKey,
		ProjectSymbols: []promptcomplete.Candidate{{
			Text: "CachedSymbol",
			Kind: "project-symbol",
		}},
		Sources: []promptContextSourceStatus{
			promptContextSourceReport(promptContextSourceProjectSymbols, promptContextFreshnessFresh, "1 candidate"),
		},
	})

	_, exactOK := cache.freshSnapshot(baseKey, now, time.Minute)
	require.True(t, exactOK)

	cases := []struct {
		name string
		key  promptContextCacheKey
	}{
		{
			name: "git",
			key: promptContextCacheKey{
				Root:            baseKey.Root,
				GitRevision:     "git-b",
				SelectedAgent:   baseKey.SelectedAgent,
				SessionRevision: baseKey.SessionRevision,
				TaskRevision:    baseKey.TaskRevision,
			},
		},
		{
			name: "session",
			key: promptContextCacheKey{
				Root:            baseKey.Root,
				GitRevision:     baseKey.GitRevision,
				SelectedAgent:   baseKey.SelectedAgent,
				SessionRevision: "session-b",
				TaskRevision:    baseKey.TaskRevision,
			},
		},
		{
			name: "task",
			key: promptContextCacheKey{
				Root:            baseKey.Root,
				GitRevision:     baseKey.GitRevision,
				SelectedAgent:   baseKey.SelectedAgent,
				SessionRevision: baseKey.SessionRevision,
				TaskRevision:    "task-b",
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			_, freshOK := cache.freshSnapshot(tc.key, now, time.Minute)
			require.False(t, freshOK)

			compatible, compatibleOK := cache.compatibleSnapshot(tc.key)
			require.True(t, compatibleOK)

			stale := stalePromptRepoContextSnapshot(compatible, tc.key, now.Add(2*time.Second))

			assert.Equal(t, tc.key, stale.Key)
			assert.Contains(t, candidateTexts(stale.ProjectSymbols), "CachedSymbol")
			assert.Equal(t, promptContextFreshnessStale, statusForSource(t, stale.Sources, promptContextSourceProjectSymbols).Status)
		})
	}
}

func TestPromptContextCache_StaleSnapshotSurfacesFailedSourcesWithoutCandidates(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 5, 26, 12, 0, 0, 0, time.UTC)
	requestedKey := promptContextCacheKey{
		Root:            "/repo",
		GitRevision:     "git-b",
		SelectedAgent:   "planner",
		SessionRevision: "session-b",
		TaskRevision:    "task-b",
	}
	snapshot := promptRepoContextSnapshot{
		CapturedAt: now,
		Key: promptContextCacheKey{
			Root:            "/repo",
			GitRevision:     "git-a",
			SelectedAgent:   "planner",
			SessionRevision: "session-a",
			TaskRevision:    "task-a",
		},
		Sources: []promptContextSourceStatus{
			promptContextSourceReport(promptContextSourceProjectSymbols, promptContextFreshnessError, "index failed"),
			promptContextSourceReport(promptContextSourceGitBranch, promptContextFreshnessSkipped, "no .git metadata"),
			promptContextSourceReport(promptContextSourceGitStatus, promptContextFreshnessFresh, "0 candidates"),
		},
	}

	stale := stalePromptRepoContextSnapshot(snapshot, requestedKey, now.Add(2*time.Second))
	projectSymbols := statusForSource(t, stale.Sources, promptContextSourceProjectSymbols)

	assert.Equal(t, requestedKey, stale.Key)
	assert.Equal(t, promptContextFreshnessError, projectSymbols.Status)
	assert.Contains(t, projectSymbols.Detail, "refreshing")
	assert.Contains(t, projectSymbols.Detail, "previous error")
	assert.Equal(t, "error:project-symbols", promptContextStatusLabel(stale.Sources))
}

func TestPromptContextCache_PrunesSnapshots(t *testing.T) {
	t.Parallel()

	cache := newPromptContextCache()
	now := time.Date(2026, 5, 26, 12, 0, 0, 0, time.UTC)

	for i := range maxPromptContextCacheSnapshots + 5 {
		root := "/repo/" + strconv.Itoa(i)
		cache.store(promptRepoContextSnapshot{
			CapturedAt: now.Add(time.Duration(i) * time.Second),
			Key: promptContextCacheKey{
				Root:          root,
				SelectedAgent: "planner",
			},
		})
	}

	require.Len(t, cache.snapshots, maxPromptContextCacheSnapshots)
	assert.NotContains(t, cache.snapshots, promptContextCacheKey{Root: "/repo/0", SelectedAgent: "planner"})
	assert.Contains(t, cache.snapshots, promptContextCacheKey{Root: "/repo/34", SelectedAgent: "planner"})
}

//nolint:paralleltest // Uses t.Chdir to verify the default task-list path has no repo side effects.
func TestPromptCompletionContext_MissingTaskListDoesNotCreateDefaultTaskDir(t *testing.T) {
	tempDir := t.TempDir()
	t.Chdir(tempDir)

	completionContext := promptCompletionContext(context.Background(), appState{}, "ask", false)

	assert.Empty(t, completionContext.Tasks)

	_, err := os.Stat(filepath.Join(tempDir, ".atteler"))
	assert.ErrorIs(t, err, os.ErrNotExist)
}

func TestPromptCompletionContext_LimitsSessionDerivedContext(t *testing.T) {
	t.Parallel()

	opts := defaultPromptCompletionContextOptions()
	opts.MaxCandidates = 2

	state := appState{
		sessionState: session.Session{
			Tags: []string{"old GH-0", "middle GH-2", "latest GH-3"},
			Messages: []llm.Message{
				{Role: llm.RoleUser, Content: "old issue GH-1"},
				{Role: llm.RoleUser, Content: "middle issue GH-2"},
				{Role: llm.RoleUser, Content: "latest issue GH-3"},
			},
			Artifacts: []session.Artifact{
				{Path: "old.md", Kind: "notes"},
				{Path: "middle.md", Kind: "notes"},
				{Path: "latest.md", Kind: "notes"},
			},
		},
	}

	result := promptCompletionContextWithOptions(context.Background(), state, "GH-", false, opts)

	assert.Len(t, result.Context.RecentFiles, 2)
	assert.NotContains(t, candidateTexts(result.Context.RecentFiles), "old.md")
	assert.Contains(t, candidateTexts(result.Context.RecentFiles), "middle.md")
	assert.Contains(t, candidateTexts(result.Context.RecentFiles), "latest.md")
	assert.NotContains(t, candidateTexts(result.Context.Issues), "GH-0")
	assert.NotContains(t, candidateTexts(result.Context.Issues), "GH-1")
	assert.Contains(t, candidateTexts(result.Context.Issues), "GH-2")
	assert.Contains(t, candidateTexts(result.Context.Issues), "GH-3")
}

func TestRunPromptSource_UncooperativeSessionSourceReturnsPartialPromptly(t *testing.T) {
	t.Parallel()

	releaseSource := make(chan struct{})

	t.Cleanup(func() {
		close(releaseSource)
	})

	start := time.Now()
	payload, status := runPromptSource(context.Background(), 10*time.Millisecond, promptContextSourceSession, func(context.Context) (promptSourcePayload, error) {
		<-releaseSource

		return promptSourcePayload{Issues: []promptcomplete.Candidate{{Text: "GH-129"}}}, nil
	})
	elapsed := time.Since(start)

	assert.Less(t, elapsed, promptContextTestReturnBudget)
	assert.Empty(t, payload.Issues)
	assert.Equal(t, promptContextSourceSession, status.Source)
	assert.Equal(t, promptContextFreshnessPartial, status.Status)
}

func TestRunPromptSource_PartialErrorKeepsPayload(t *testing.T) {
	t.Parallel()

	payload, status := runPromptSource(context.Background(), time.Second, promptContextSourceGitStatus, func(context.Context) (promptSourcePayload, error) {
		return promptSourcePayload{
			RecentFiles: []promptcomplete.Candidate{{
				Text: "kept.go",
				Kind: "recent-file",
			}},
		}, promptContextPartialError{reason: "git output exceeded 10 bytes per stream"}
	})

	assert.Equal(t, []string{"kept.go"}, candidateTexts(payload.RecentFiles))
	assert.Equal(t, promptContextSourceGitStatus, status.Source)
	assert.Equal(t, promptContextFreshnessPartial, status.Status)
	assert.Contains(t, status.Detail, "git output exceeded")
}

func TestPromptContextCacheKey_ChangesOnSessionTaskGitAndAgentRevisions(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(dir, ".git", "refs", "heads"), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(dir, ".git", "HEAD"), []byte("ref: refs/heads/main\n"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, ".git", "refs", "heads", "main"), []byte("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa\n"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, ".git", "index"), []byte("index-v1"), 0o600))

	store := session.NewStore(filepath.Join(dir, ".atteler", "sessions"))
	state := appState{
		cwd:           dir,
		sessionStore:  store,
		selectedAgent: "planner",
		sessionState: session.Session{
			ID:    "session-1",
			Title: "Work on GH-129",
		},
	}

	base := promptContextCacheKeyForState(state)

	sessionChanged := state
	sessionChanged.sessionState.Title = "Work on GH-124"
	assert.NotEqual(t, base.SessionRevision, promptContextCacheKeyForState(sessionChanged).SessionRevision)

	taskStore := tasklist.NewStore(taskListPath(store, ""))
	_, err := taskStore.Add(context.Background(), tasklist.AddRequest{ID: "GH-129", Title: "Cache prompt context"})
	require.NoError(t, err)
	assert.NotEqual(t, base.TaskRevision, promptContextCacheKeyForState(state).TaskRevision)

	require.NoError(t, os.WriteFile(filepath.Join(dir, ".git", "refs", "heads", "main"), []byte("bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb\n"), 0o600))
	assert.NotEqual(t, base.GitRevision, promptContextCacheKeyForState(state).GitRevision)

	agentChanged := state
	agentChanged.selectedAgent = "executor"
	assert.NotEqual(t, base.SelectedAgent, promptContextCacheKeyForState(agentChanged).SelectedAgent)
}

func TestPromptSessionRevision_ChangesOnLiveMessageAppendWithoutUpdatedAt(t *testing.T) {
	t.Parallel()

	sessionState := session.Session{
		ID: "session-1",
		Messages: []llm.Message{{
			Role:    llm.RoleUser,
			Content: "first message",
		}},
	}

	base := promptSessionRevision(sessionState)

	sessionState.Messages = append(sessionState.Messages, llm.Message{
		Role:    llm.RoleAssistant,
		Content: "second message",
	})

	assert.NotEqual(t, base, promptSessionRevision(sessionState))
}

func TestPromptGitRevision_ChangesWhenCommonDirRefChanges(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	root := filepath.Join(dir, "worktree")
	commonDir := filepath.Join(dir, "common")
	gitDir := filepath.Join(commonDir, "worktrees", "worktree")
	refPath := filepath.Join(commonDir, "refs", "heads", "main")

	require.NoError(t, os.MkdirAll(root, 0o700))
	require.NoError(t, os.MkdirAll(gitDir, 0o700))
	require.NoError(t, os.MkdirAll(filepath.Dir(refPath), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(root, ".git"), []byte("gitdir: ../common/worktrees/worktree\n"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(gitDir, "commondir"), []byte("../..\n"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(gitDir, "HEAD"), []byte("ref: refs/heads/main\n"), 0o600))
	require.NoError(t, os.WriteFile(refPath, []byte("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa\n"), 0o600))

	base := promptGitRevision(root)

	require.NoError(t, os.WriteFile(refPath, []byte("bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb\n"), 0o600))

	assert.NotEqual(t, base, promptGitRevision(root))
}

func TestPromptGitRevision_ChangesWhenPackedRefsChange(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	packedRefsPath := filepath.Join(dir, ".git", "packed-refs")

	require.NoError(t, os.MkdirAll(filepath.Join(dir, ".git"), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(dir, ".git", "HEAD"), []byte("ref: refs/heads/main\n"), 0o600))
	require.NoError(t, os.WriteFile(packedRefsPath, []byte("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa refs/heads/main\n"), 0o600))

	packedAt := time.Date(2026, 5, 26, 12, 0, 0, 0, time.UTC)
	require.NoError(t, os.Chtimes(packedRefsPath, packedAt, packedAt))

	base := promptGitRevision(dir)

	require.NoError(t, os.WriteFile(packedRefsPath, []byte("bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb refs/heads/main\n"), 0o600))
	require.NoError(t, os.Chtimes(packedRefsPath, packedAt.Add(time.Second), packedAt.Add(time.Second)))

	assert.NotEqual(t, base, promptGitRevision(dir))
}

func TestPromptGitRevision_ChangesWhenIndexChanges(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	indexPath := filepath.Join(dir, ".git", "index")

	require.NoError(t, os.MkdirAll(filepath.Join(dir, ".git"), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(dir, ".git", "HEAD"), []byte("deadbeef\n"), 0o600))
	require.NoError(t, os.WriteFile(indexPath, []byte("index-v1"), 0o600))

	indexAt := time.Date(2026, 5, 26, 12, 0, 0, 0, time.UTC)
	require.NoError(t, os.Chtimes(indexPath, indexAt, indexAt))

	base := promptGitRevision(dir)

	require.NoError(t, os.WriteFile(indexPath, []byte("index-v2-with-more-data"), 0o600))
	require.NoError(t, os.Chtimes(indexPath, indexAt.Add(time.Second), indexAt.Add(time.Second)))

	assert.NotEqual(t, base, promptGitRevision(dir))
}

//nolint:paralleltest // Uses process-wide prompt context source hooks.
func TestPromptCompletionContext_IndexesRepositoryRootFromSubdirectory(t *testing.T) {
	dir := t.TempDir()
	subdir := filepath.Join(dir, "cmd", "atteler")

	require.NoError(t, os.MkdirAll(filepath.Join(dir, ".git"), 0o700))
	require.NoError(t, os.MkdirAll(subdir, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "root.go"), []byte("package root\n\nfunc RootSymbol() {}\n"), 0o600))

	indexedRoot := ""
	restoreIndex := setPromptCodeIndexDirContextForTest(func(_ context.Context, root string) (codeintel.Index, error) {
		indexedRoot = root

		return codeintel.Index{Symbols: []codeintel.Symbol{{
			Name: "RootSymbol",
			Kind: "func",
			File: filepath.Join(root, "root.go"),
			Line: 3,
		}}}, nil
	})
	t.Cleanup(restoreIndex)

	restoreGit := setPromptGitOutputFuncForTest(func(_ context.Context, _ string, _ ...string) (string, error) {
		return "", nil
	})
	t.Cleanup(restoreGit)

	state := appState{cwd: subdir, promptContextCache: newPromptContextCache()}
	result := promptCompletionContextWithOptions(context.Background(), state, "Root", true, defaultPromptCompletionContextOptions())

	assert.Equal(t, filepath.Clean(dir), indexedRoot)
	assert.Equal(t, filepath.Clean(dir), promptContextCacheKeyForState(state).Root)
	assert.Contains(t, candidateTexts(result.Context.ProjectSymbols), "RootSymbol")
}

//nolint:paralleltest // Uses process-wide prompt context source hooks.
func TestPromptCompletionContext_UsesConfiguredContextRootForRepoCache(t *testing.T) {
	outer := t.TempDir()
	repoRoot := filepath.Join(outer, "worktree")
	cwd := filepath.Join(outer, "launcher")

	require.NoError(t, os.MkdirAll(cwd, 0o700))
	require.NoError(t, os.MkdirAll(repoRoot, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(repoRoot, "repo.go"), []byte("package repo\n\nfunc ContextRootSymbol() {}\n"), 0o600))

	restoreIndex := setPromptCodeIndexDirContextForTest(func(_ context.Context, root string) (codeintel.Index, error) {
		require.Equal(t, filepath.Clean(repoRoot), root)

		return codeintel.Index{Symbols: []codeintel.Symbol{{
			Name: "ContextRootSymbol",
			Kind: "func",
			File: filepath.Join(root, "repo.go"),
			Line: 3,
		}}}, nil
	})
	t.Cleanup(restoreIndex)

	state := appState{
		cwd:                cwd,
		contextOptions:     contextref.Options{Root: repoRoot},
		promptContextCache: newPromptContextCache(),
	}
	result := promptCompletionContextWithOptions(context.Background(), state, "ContextRoot", true, defaultPromptCompletionContextOptions())

	assert.Contains(t, candidateTexts(result.Context.ProjectSymbols), "ContextRootSymbol")
	assert.Equal(t, filepath.Clean(repoRoot), promptContextCacheKeyForState(state).Root)
}

func TestPromptContextRootForState_PrefersWorktreeOverDefaultContextRoot(t *testing.T) {
	t.Parallel()

	outer := t.TempDir()
	cwd := filepath.Join(outer, "source")
	worktreeRoot := filepath.Join(outer, "session-worktree")

	require.NoError(t, os.MkdirAll(cwd, 0o700))
	require.NoError(t, os.MkdirAll(worktreeRoot, 0o700))

	state := appState{
		cwd:            cwd,
		contextOptions: contextref.Options{Root: cwd + string(os.PathSeparator)},
		worktreeInfo:   &worktree.Info{Path: worktreeRoot},
	}

	assert.Equal(t, filepath.Clean(worktreeRoot), promptContextRootForState(state))
	assert.Equal(t, filepath.Clean(worktreeRoot), promptContextCacheKeyForState(state).Root)
}

//nolint:paralleltest // Uses process-wide prompt context source hooks.
func TestPromptCompletionContext_ProjectSymbolTimeoutReturnsPartialPromptly(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "slow.go"), []byte("package slow\n\nfunc SlowSymbol() {}\n"), 0o600))

	t.Cleanup(setPromptCodeIndexDirContextForTest(func(ctx context.Context, _ string) (codeintel.Index, error) {
		<-ctx.Done()

		return codeintel.Index{}, ctx.Err()
	}))

	opts := defaultPromptCompletionContextOptions()
	opts.IndexTimeout = 10 * time.Millisecond
	opts.GitTimeout = 10 * time.Millisecond
	state := appState{cwd: dir, promptContextCache: newPromptContextCache()}

	start := time.Now()
	result := promptCompletionContextWithOptions(context.Background(), state, "Slow", true, opts)
	elapsed := time.Since(start)

	assert.Less(t, elapsed, promptContextTestReturnBudget)
	assert.Empty(t, result.Context.ProjectSymbols)
	assert.Equal(t, promptContextFreshnessPartial, statusForSource(t, result.Sources, promptContextSourceProjectSymbols).Status)
}

func TestPromptCompletionContext_InteractiveSkipsEmptyRootWithoutWarming(t *testing.T) {
	t.Parallel()

	result := promptCompletionContextInteractive(context.Background(), appState{}, "Repo", true)
	projectSymbols := statusForSource(t, result.Sources, promptContextSourceProjectSymbols)

	assert.Equal(t, promptContextFreshnessSkipped, projectSymbols.Status)
	assert.Contains(t, projectSymbols.Detail, "repository root is empty")
}

func TestPromptContextCache_EmptyRootSnapshotsAreNotStoredOrReturnedAsStale(t *testing.T) {
	t.Parallel()

	cache := newPromptContextCache()
	cache.store(promptRepoContextSnapshot{
		CapturedAt: time.Now(),
		Key: promptContextCacheKey{
			Root:          "",
			SelectedAgent: "planner",
		},
		Sources: []promptContextSourceStatus{
			promptContextSourceReport(promptContextSourceProjectSymbols, promptContextFreshnessSkipped, "repository root is empty"),
		},
	})

	_, ok := cache.compatibleSnapshot(promptContextCacheKey{SelectedAgent: "planner"})

	assert.False(t, ok)
	assert.Empty(t, cache.snapshots)
}

//nolint:paralleltest // Uses process-wide prompt context source hooks.
func TestPromptCompletionContext_UncooperativeProjectIndexReturnsPartialPromptly(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "stuck.go"), []byte("package stuck\n\nfunc StuckSymbol() {}\n"), 0o600))

	releaseIndex := make(chan struct{})
	restoreIndex := setPromptCodeIndexDirContextForTest(func(context.Context, string) (codeintel.Index, error) {
		<-releaseIndex

		return codeintel.Index{}, nil
	})
	t.Cleanup(restoreIndex)
	t.Cleanup(func() {
		close(releaseIndex)
	})

	opts := defaultPromptCompletionContextOptions()
	opts.IndexTimeout = 10 * time.Millisecond
	state := appState{cwd: dir, promptContextCache: newPromptContextCache()}

	start := time.Now()
	result := promptCompletionContextWithOptions(context.Background(), state, "Stuck", true, opts)
	elapsed := time.Since(start)

	assert.Less(t, elapsed, promptContextTestReturnBudget)
	assert.Empty(t, result.Context.ProjectSymbols)
	assert.Equal(t, promptContextFreshnessPartial, statusForSource(t, result.Sources, promptContextSourceProjectSymbols).Status)
}

//nolint:paralleltest // Uses process-wide prompt context source hooks.
func TestPromptCompletionContext_ProjectSymbolFileLimitSkipsIndex(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "one.go"), []byte("package many\n"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "two.go"), []byte("package many\n"), 0o600))

	indexCalls := 0
	restoreIndex := setPromptCodeIndexDirContextForTest(func(_ context.Context, _ string) (codeintel.Index, error) {
		indexCalls++

		return codeintel.Index{}, nil
	})
	t.Cleanup(restoreIndex)

	opts := defaultPromptCompletionContextOptions()
	opts.MaxIndexFiles = 1
	state := appState{cwd: dir, promptContextCache: newPromptContextCache()}

	result := promptCompletionContextWithOptions(context.Background(), state, "Many", true, opts)
	projectSymbols := statusForSource(t, result.Sources, promptContextSourceProjectSymbols)

	assert.Equal(t, 0, indexCalls)
	assert.Empty(t, result.Context.ProjectSymbols)
	assert.Equal(t, promptContextFreshnessSkipped, projectSymbols.Status)
	assert.Contains(t, projectSymbols.Detail, "go file count exceeds limit 1")
}

//nolint:paralleltest // Uses process-wide prompt context source hooks.
func TestPromptCompletionContext_GitTimeoutReturnsPartialPromptly(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.Mkdir(filepath.Join(dir, ".git"), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(dir, ".git", "HEAD"), []byte("deadbeef\n"), 0o600))

	t.Cleanup(setPromptGitOutputFuncForTest(func(ctx context.Context, _ string, _ ...string) (string, error) {
		<-ctx.Done()

		return "", ctx.Err()
	}))

	opts := defaultPromptCompletionContextOptions()
	opts.GitTimeout = 10 * time.Millisecond
	opts.IndexTimeout = 10 * time.Millisecond
	state := appState{cwd: dir, promptContextCache: newPromptContextCache()}

	start := time.Now()
	result := promptCompletionContextWithOptions(context.Background(), state, "GH-", true, opts)
	elapsed := time.Since(start)

	assert.Less(t, elapsed, promptContextTestReturnBudget)
	assert.Equal(t, promptContextFreshnessPartial, statusForSource(t, result.Sources, promptContextSourceGitBranch).Status)
	assert.Equal(t, promptContextFreshnessPartial, statusForSource(t, result.Sources, promptContextSourceGitStatus).Status)
}

//nolint:paralleltest // Uses process-wide prompt context source hooks.
func TestPromptCompletionContext_UncooperativeGitReturnsPartialPromptly(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.Mkdir(filepath.Join(dir, ".git"), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(dir, ".git", "HEAD"), []byte("deadbeef\n"), 0o600))

	releaseGit := make(chan struct{})

	t.Cleanup(func() {
		close(releaseGit)
	})

	t.Cleanup(setPromptGitOutputFuncForTest(func(context.Context, string, ...string) (string, error) {
		<-releaseGit

		return "", nil
	}))

	opts := defaultPromptCompletionContextOptions()
	opts.GitTimeout = 10 * time.Millisecond
	opts.IndexTimeout = 10 * time.Millisecond
	state := appState{cwd: dir, promptContextCache: newPromptContextCache()}

	start := time.Now()
	result := promptCompletionContextWithOptions(context.Background(), state, "GH-", true, opts)
	elapsed := time.Since(start)

	assert.Less(t, elapsed, promptContextTestReturnBudget)
	assert.Equal(t, promptContextFreshnessPartial, statusForSource(t, result.Sources, promptContextSourceGitBranch).Status)
	assert.Equal(t, promptContextFreshnessPartial, statusForSource(t, result.Sources, promptContextSourceGitStatus).Status)
}

//nolint:paralleltest // Uses process-wide prompt context source hooks.
func TestPromptCompletionContext_TaskTimeoutReturnsPartialPromptly(t *testing.T) {
	dir := t.TempDir()
	store := session.NewStore(filepath.Join(dir, ".atteler", "sessions"))
	taskList := taskListPath(store, "")
	require.NoError(t, os.MkdirAll(filepath.Dir(taskList), 0o700))
	require.NoError(t, os.WriteFile(taskList, []byte("[]"), 0o600))

	t.Cleanup(setPromptTaskCandidatesWithErrorForTest(func(ctx context.Context, _ *session.Store, _ string, _ int) ([]promptcomplete.Candidate, error) {
		<-ctx.Done()

		return nil, ctx.Err()
	}))

	opts := defaultPromptCompletionContextOptions()
	opts.TaskTimeout = 10 * time.Millisecond
	state := appState{cwd: dir, sessionStore: store}

	start := time.Now()
	result := promptCompletionContextWithOptions(context.Background(), state, "GH-", false, opts)
	elapsed := time.Since(start)

	assert.Less(t, elapsed, promptContextTestReturnBudget)
	assert.Empty(t, result.Context.Tasks)
	assert.Equal(t, promptContextFreshnessPartial, statusForSource(t, result.Sources, promptContextSourceTasks).Status)
}

//nolint:paralleltest // Uses process-wide prompt context source hooks.
func TestPromptCompletionContext_UncooperativeTaskLoadReturnsPartialPromptly(t *testing.T) {
	dir := t.TempDir()
	store := session.NewStore(filepath.Join(dir, ".atteler", "sessions"))
	taskList := taskListPath(store, "")
	require.NoError(t, os.MkdirAll(filepath.Dir(taskList), 0o700))
	require.NoError(t, os.WriteFile(taskList, []byte("[]"), 0o600))

	releaseTasks := make(chan struct{})

	t.Cleanup(func() {
		close(releaseTasks)
	})

	t.Cleanup(setPromptTaskCandidatesWithErrorForTest(func(context.Context, *session.Store, string, int) ([]promptcomplete.Candidate, error) {
		<-releaseTasks

		return nil, nil
	}))

	opts := defaultPromptCompletionContextOptions()
	opts.TaskTimeout = 10 * time.Millisecond
	state := appState{cwd: dir, sessionStore: store}

	start := time.Now()
	result := promptCompletionContextWithOptions(context.Background(), state, "GH-", false, opts)
	elapsed := time.Since(start)

	assert.Less(t, elapsed, promptContextTestReturnBudget)
	assert.Empty(t, result.Context.Tasks)
	assert.Equal(t, promptContextFreshnessPartial, statusForSource(t, result.Sources, promptContextSourceTasks).Status)
}

//nolint:paralleltest // Uses process-wide prompt context source hooks.
func TestPromptCompletionContext_UsesStaleRepoSnapshotWhileRefreshing(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "fast.go"), []byte("package fast\n\nfunc FastSymbol() {}\n"), 0o600))

	restoreIndex := setPromptCodeIndexDirContextForTest(func(_ context.Context, root string) (codeintel.Index, error) {
		return codeintel.Index{Symbols: []codeintel.Symbol{{
			Name: "FastSymbol",
			Kind: "func",
			File: filepath.Join(root, "fast.go"),
			Line: 3,
		}}}, nil
	})
	t.Cleanup(restoreIndex)

	now := time.Date(2026, 5, 26, 12, 0, 0, 0, time.UTC)
	opts := defaultPromptCompletionContextOptions()
	opts.Now = func() time.Time { return now }
	opts.CacheTTL = time.Second
	state := appState{cwd: dir, promptContextCache: newPromptContextCache()}

	first := promptCompletionContextWithOptions(context.Background(), state, "Fast", true, opts)
	require.Contains(t, candidateTexts(first.Context.ProjectSymbols), "FastSymbol")
	assert.Equal(t, promptContextFreshnessFresh, statusForSource(t, first.Sources, promptContextSourceProjectSymbols).Status)

	var refreshCalls atomic.Int32

	refreshDone := make(chan struct{}, 1)

	setPromptCodeIndexDirContextForTest(func(ctx context.Context, _ string) (codeintel.Index, error) {
		refreshCalls.Add(1)

		defer func() {
			select {
			case refreshDone <- struct{}{}:
			default:
			}
		}()

		<-ctx.Done()

		return codeintel.Index{}, ctx.Err()
	})

	now = now.Add(2 * time.Second)
	opts.WaitForRepo = false
	opts.IndexTimeout = 10 * time.Millisecond

	start := time.Now()
	second := promptCompletionContextWithOptions(context.Background(), state, "Fast", true, opts)
	elapsed := time.Since(start)

	assert.Less(t, elapsed, promptContextTestReturnBudget)
	assert.Contains(t, candidateTexts(second.Context.ProjectSymbols), "FastSymbol")
	assert.Equal(t, promptContextFreshnessStale, statusForSource(t, second.Sources, promptContextSourceProjectSymbols).Status)

	select {
	case <-refreshDone:
	case <-time.After(promptContextTestBackgroundBudget):
		require.Fail(t, "background refresh did not honor the source timeout")
	}

	var third promptCompletionContextResult

	require.Eventually(t, func() bool {
		third = promptCompletionContextWithOptions(context.Background(), state, "Fast", true, opts)
		projectSymbols := statusForSource(t, third.Sources, promptContextSourceProjectSymbols)

		return strings.Contains(projectSymbols.Detail, "refresh partial")
	}, promptContextTestBackgroundBudget, 10*time.Millisecond)

	assert.Contains(t, candidateTexts(third.Context.ProjectSymbols), "FastSymbol")
	projectSymbols := statusForSource(t, third.Sources, promptContextSourceProjectSymbols)
	assert.Equal(t, promptContextFreshnessStale, projectSymbols.Status)
	assert.Contains(t, projectSymbols.Detail, "refresh partial")
	assert.Equal(t, int32(1), refreshCalls.Load(), "stale exact snapshots should respect TTL instead of refreshing every idle cycle")
}

//nolint:paralleltest // Uses process-wide prompt context source hooks.
func TestPromptCompletionContext_InteractiveWarmsCacheWithoutBlocking(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "slow.go"), []byte("package slow\n\nfunc SlowSymbol() {}\n"), 0o600))

	indexDone := make(chan struct{})
	restoreIndex := setPromptCodeIndexDirContextForTest(func(ctx context.Context, _ string) (codeintel.Index, error) {
		defer close(indexDone)

		<-ctx.Done()

		return codeintel.Index{}, ctx.Err()
	})
	t.Cleanup(restoreIndex)

	opts := defaultPromptCompletionContextOptions()
	opts.WaitForRepo = false
	opts.IndexTimeout = 10 * time.Millisecond
	state := appState{cwd: dir, promptContextCache: newPromptContextCache()}

	start := time.Now()
	result := promptCompletionContextWithOptions(context.Background(), state, "Slow", true, opts)
	elapsed := time.Since(start)

	assert.Less(t, elapsed, promptContextTestReturnBudget)
	assert.Empty(t, result.Context.ProjectSymbols)

	projectSymbols := statusForSource(t, result.Sources, promptContextSourceProjectSymbols)
	assert.Equal(t, promptContextFreshnessSkipped, projectSymbols.Status)
	assert.Contains(t, projectSymbols.Detail, "warming")

	select {
	case <-indexDone:
	case <-time.After(promptContextTestBackgroundBudget):
		require.Fail(t, "background cache warmup did not honor the source timeout")
	}
}

//nolint:paralleltest // Uses process-wide prompt context source hooks.
func TestPromptCompletionContext_InteractivePartialWarmupRespectsTTL(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "retry.go"), []byte("package retry\n\nfunc RetrySymbol() {}\n"), 0o600))

	var indexCalls atomic.Int32

	firstDone := make(chan struct{})
	retryStarted := make(chan struct{}, 1)
	restoreIndex := setPromptCodeIndexDirContextForTest(func(ctx context.Context, root string) (codeintel.Index, error) {
		switch indexCalls.Add(1) {
		case 1:
			defer close(firstDone)

			<-ctx.Done()

			return codeintel.Index{}, ctx.Err()
		default:
			select {
			case retryStarted <- struct{}{}:
			default:
			}

			return codeintel.Index{Symbols: []codeintel.Symbol{{
				Name: "RetrySymbol",
				Kind: "func",
				File: filepath.Join(root, "retry.go"),
				Line: 3,
			}}}, nil
		}
	})
	t.Cleanup(restoreIndex)

	now := time.Date(2026, 5, 26, 12, 0, 0, 0, time.UTC)
	opts := defaultPromptCompletionContextOptions()
	opts.Now = func() time.Time { return now }
	opts.CacheTTL = time.Minute
	opts.WaitForRepo = false
	opts.IndexTimeout = 10 * time.Millisecond
	state := appState{cwd: dir, promptContextCache: newPromptContextCache()}

	first := promptCompletionContextWithOptions(context.Background(), state, "Retry", true, opts)
	assert.Empty(t, first.Context.ProjectSymbols)
	assert.Equal(t, promptContextFreshnessSkipped, statusForSource(t, first.Sources, promptContextSourceProjectSymbols).Status)

	select {
	case <-firstDone:
	case <-time.After(promptContextTestBackgroundBudget):
		require.Fail(t, "first cache warmup did not finish")
	}

	start := time.Now()
	second := promptCompletionContextWithOptions(context.Background(), state, "Retry", true, opts)
	elapsed := time.Since(start)

	assert.Less(t, elapsed, promptContextTestReturnBudget)
	assert.Empty(t, second.Context.ProjectSymbols)
	assert.Equal(t, promptContextFreshnessPartial, statusForSource(t, second.Sources, promptContextSourceProjectSymbols).Status)
	assert.Equal(t, int32(1), indexCalls.Load(), "partial warmup snapshots should be reused until their TTL expires")

	now = now.Add(time.Minute + time.Second)

	third := promptCompletionContextWithOptions(context.Background(), state, "Retry", true, opts)
	assert.Empty(t, third.Context.ProjectSymbols)
	assert.Equal(t, promptContextFreshnessPartial, statusForSource(t, third.Sources, promptContextSourceProjectSymbols).Status)

	select {
	case <-retryStarted:
	case <-time.After(promptContextTestBackgroundBudget):
		require.Fail(t, "partial cache snapshot did not retry after TTL")
	}

	var fourth promptCompletionContextResult

	require.Eventually(t, func() bool {
		fourth = promptCompletionContextWithOptions(context.Background(), state, "Retry", true, opts)

		return slices.Contains(candidateTexts(fourth.Context.ProjectSymbols), "RetrySymbol")
	}, promptContextTestBackgroundBudget, 10*time.Millisecond)

	assert.Equal(t, int32(2), indexCalls.Load())
	assert.Equal(t, promptContextFreshnessFresh, statusForSource(t, fourth.Sources, promptContextSourceProjectSymbols).Status)
}

//nolint:paralleltest // Uses process-wide prompt context source hooks.
func TestPromptCompletionContext_InteractiveWarmupDeduplicatesConcurrentRefresh(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "slow.go"), []byte("package slow\n\nfunc SlowSymbol() {}\n"), 0o600))

	var (
		indexCalls  atomic.Int32
		releaseOnce sync.Once
	)

	indexStarted := make(chan struct{}, 1)
	indexDone := make(chan struct{}, 1)
	releaseIndex := make(chan struct{})

	restoreIndex := setPromptCodeIndexDirContextForTest(func(context.Context, string) (codeintel.Index, error) {
		indexCalls.Add(1)

		defer func() {
			indexDone <- struct{}{}
		}()

		select {
		case indexStarted <- struct{}{}:
		default:
		}

		<-releaseIndex

		return codeintel.Index{}, nil
	})
	t.Cleanup(restoreIndex)
	t.Cleanup(func() {
		releaseOnce.Do(func() {
			close(releaseIndex)
		})
	})

	opts := defaultPromptCompletionContextOptions()
	opts.WaitForRepo = false
	opts.IndexTimeout = promptContextTestBackgroundBudget
	state := appState{cwd: dir, promptContextCache: newPromptContextCache()}

	first := promptCompletionContextWithOptions(context.Background(), state, "Slow", true, opts)
	second := promptCompletionContextWithOptions(context.Background(), state, "Slow", true, opts)

	assert.Equal(t, promptContextFreshnessSkipped, statusForSource(t, first.Sources, promptContextSourceProjectSymbols).Status)
	assert.Equal(t, promptContextFreshnessSkipped, statusForSource(t, second.Sources, promptContextSourceProjectSymbols).Status)

	select {
	case <-indexStarted:
	case <-time.After(promptContextTestBackgroundBudget):
		require.Fail(t, "background cache warmup did not start")
	}

	assert.Equal(t, int32(1), indexCalls.Load())

	releaseOnce.Do(func() {
		close(releaseIndex)
	})

	select {
	case <-indexDone:
	case <-time.After(promptContextTestBackgroundBudget):
		require.Fail(t, "background cache warmup did not stop")
	}
}

//nolint:paralleltest // Uses process-wide prompt context source hooks.
func TestPromptCompletionContext_FreshRepoSnapshotAvoidsReindex(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "fast.go"), []byte("package fast\n\nfunc FastSymbol() {}\n"), 0o600))

	indexCalls := 0
	restoreIndex := setPromptCodeIndexDirContextForTest(func(_ context.Context, root string) (codeintel.Index, error) {
		indexCalls++

		return codeintel.Index{Symbols: []codeintel.Symbol{{
			Name: "FastSymbol",
			Kind: "func",
			File: filepath.Join(root, "fast.go"),
			Line: 3,
		}}}, nil
	})
	t.Cleanup(restoreIndex)

	now := time.Date(2026, 5, 26, 12, 0, 0, 0, time.UTC)
	opts := defaultPromptCompletionContextOptions()
	opts.Now = func() time.Time { return now }
	opts.CacheTTL = time.Minute
	state := appState{cwd: dir, promptContextCache: newPromptContextCache()}

	first := promptCompletionContextWithOptions(context.Background(), state, "Fast", true, opts)
	second := promptCompletionContextWithOptions(context.Background(), state, "Fast", true, opts)

	require.Contains(t, candidateTexts(first.Context.ProjectSymbols), "FastSymbol")
	require.Contains(t, candidateTexts(second.Context.ProjectSymbols), "FastSymbol")
	assert.Equal(t, 1, indexCalls)
	assert.Equal(t, promptContextFreshnessFresh, statusForSource(t, second.Sources, promptContextSourceProjectSymbols).Status)
}

//nolint:paralleltest // Uses process-wide prompt context source hooks.
func TestPromptCompletionContext_FiltersCachedSymbolsBeyondFinalCandidateLimit(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "symbols.go"), []byte("package symbols\n\nfunc TargetSymbol() {}\n"), 0o600))

	restoreIndex := setPromptCodeIndexDirContextForTest(func(_ context.Context, root string) (codeintel.Index, error) {
		symbols := make([]codeintel.Symbol, 0, maxPromptContextCandidates+1)
		for i := range maxPromptContextCandidates {
			symbols = append(symbols, codeintel.Symbol{
				Name: "OtherSymbol" + strconv.Itoa(i),
				Kind: "func",
				File: filepath.Join(root, "symbols.go"),
				Line: i + 1,
			})
		}

		symbols = append(symbols, codeintel.Symbol{
			Name: "TargetSymbol",
			Kind: "func",
			File: filepath.Join(root, "symbols.go"),
			Line: maxPromptContextCandidates + 1,
		})

		return codeintel.Index{Symbols: symbols}, nil
	})
	t.Cleanup(restoreIndex)

	state := appState{cwd: dir, promptContextCache: newPromptContextCache()}
	result := promptCompletionContextWithOptions(context.Background(), state, "Target", true, defaultPromptCompletionContextOptions())

	assert.Contains(t, candidateTexts(result.Context.ProjectSymbols), "TargetSymbol")
}

//nolint:paralleltest // Uses process-wide prompt context source hooks.
func TestPromptCompletionContext_PersistsRepoSnapshotAcrossCaches(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "persist.go"), []byte("package persist\n\nfunc PersistedSymbol() {}\n"), 0o600))

	indexCalls := 0
	restoreIndex := setPromptCodeIndexDirContextForTest(func(_ context.Context, root string) (codeintel.Index, error) {
		indexCalls++

		return codeintel.Index{Symbols: []codeintel.Symbol{{
			Name: "PersistedSymbol",
			Kind: "func",
			File: filepath.Join(root, "persist.go"),
			Line: 3,
		}}}, nil
	})
	t.Cleanup(restoreIndex)

	now := time.Date(2026, 5, 26, 12, 0, 0, 0, time.UTC)
	opts := defaultPromptCompletionContextOptions()
	opts.Now = func() time.Time { return now }
	opts.CacheTTL = time.Minute

	cachePath := filepath.Join(dir, ".atteler", promptContextCacheFileName)
	firstState := appState{cwd: dir, promptContextCache: newPromptContextCache(cachePath)}
	first := promptCompletionContextWithOptions(context.Background(), firstState, "Persisted", true, opts)
	require.Contains(t, candidateTexts(first.Context.ProjectSymbols), "PersistedSymbol")

	secondState := appState{cwd: dir, promptContextCache: newPromptContextCache(cachePath)}
	second := promptCompletionContextWithOptions(context.Background(), secondState, "Persisted", true, opts)

	require.Contains(t, candidateTexts(second.Context.ProjectSymbols), "PersistedSymbol")
	assert.Equal(t, 1, indexCalls)
	assert.FileExists(t, cachePath)
}

//nolint:paralleltest // Captures process stdout and uses process-wide prompt context source hooks.
func TestPromptComplete_PrintsContextFreshness(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "cli.go"), []byte("package cli\n\nfunc CliSymbol() {}\n"), 0o600))

	restoreIndex := setPromptCodeIndexDirContextForTest(func(_ context.Context, root string) (codeintel.Index, error) {
		return codeintel.Index{Symbols: []codeintel.Symbol{{
			Name: "CliSymbol",
			Kind: "func",
			File: filepath.Join(root, "cli.go"),
			Line: 3,
		}}}, nil
	})
	t.Cleanup(restoreIndex)

	state := appState{cwd: dir, promptContextCache: newPromptContextCache()}

	out := captureStdoutForStateDiagnostics(t, func() {
		promptComplete(context.Background(), state, "Cli", 1)
	})

	assert.Contains(t, out, "context:\n")
	assert.Contains(t, out, "project symbols: fresh")
	assert.Contains(t, out, "text: CliSymbol")
	assert.Contains(t, out, "kind: project-symbol")
}

//nolint:paralleltest // Captures process stdout.
func TestPromptCompleteProviderless_UsesSessionDefaultAgentForSharedCache(t *testing.T) {
	dir := t.TempDir()
	store := session.NewStore(filepath.Join(dir, ".atteler", "sessions"))
	saved := session.New("", nil)
	saved.DefaultAgent = testReviewerName
	require.NoError(t, store.Save(saved))

	saved, err := store.Load(saved.ID)
	require.NoError(t, err)

	cache := newPromptContextCache()
	state := appState{
		agentRegistry: agent.NewRegistry(map[string]config.AgentConfig{
			testReviewerName: {Description: "reviews prompts"},
		}),
		cwd:                dir,
		sessionStore:       store,
		promptContextCache: cache,
	}

	key := promptContextCacheKeyForState(appState{
		agentRegistry:      state.agentRegistry,
		cwd:                dir,
		sessionStore:       store,
		sessionState:       saved,
		selectedAgent:      testReviewerName,
		promptContextCache: cache,
	})
	cache.store(promptRepoContextSnapshot{
		CapturedAt: time.Now(),
		Key:        key,
		ProjectSymbols: []promptcomplete.Candidate{{
			Text:        "SessionDefaultSymbol",
			Kind:        "project-symbol",
			Source:      "project symbol index",
			Description: "cached from matching session default agent",
		}},
		Sources: []promptContextSourceStatus{
			promptContextSourceReport(promptContextSourceProjectSymbols, promptContextFreshnessFresh, "1 candidate"),
		},
	})

	var promptCompleteCommand command

	for _, candidate := range buildCommandRegistry() {
		if candidate.name == "prompt-complete-providerless" {
			promptCompleteCommand = candidate
			break
		}
	}

	require.NotNil(t, promptCompleteCommand.runProviderlessConfig)

	out := captureStdoutForStateDiagnostics(t, func() {
		err := promptCompleteCommand.runProviderlessConfig(context.Background(), cliOptions{
			sessionRef:          saved.ID,
			promptCompleteInput: "SessionDefault",
			promptCompleteLimit: positiveIntFlag{value: 1},
		}, state)
		require.NoError(t, err)
	})

	assert.Contains(t, out, "project symbols: fresh")
	assert.Contains(t, out, "text: SessionDefaultSymbol")
}

//nolint:paralleltest // Captures process stdout.
func TestPromptComplete_PrintsStaleContextFreshnessWhenRefreshCannotReplaceIt(t *testing.T) {
	dir := t.TempDir()
	cache := newPromptContextCache()
	state := appState{cwd: dir, promptContextCache: cache}
	key := promptContextCacheKeyForState(state)
	cache.store(promptRepoContextSnapshot{
		CapturedAt: time.Now().Add(-time.Minute),
		Key:        key,
		ProjectSymbols: []promptcomplete.Candidate{{
			Text:   "StaleSymbol",
			Kind:   "project-symbol",
			Source: "project symbol index",
		}},
		Sources: []promptContextSourceStatus{
			promptContextSourceReport(promptContextSourceGitBranch, promptContextFreshnessSkipped, "no .git metadata"),
			promptContextSourceReport(promptContextSourceGitStatus, promptContextFreshnessSkipped, "no .git metadata"),
			promptContextSourceReport(promptContextSourceProjectSymbols, promptContextFreshnessFresh, "1 candidate"),
		},
	})

	out := captureStdoutForStateDiagnostics(t, func() {
		promptComplete(context.Background(), state, "Stale", 1)
	})

	assert.Contains(t, out, "context:\n")
	assert.Contains(t, out, "git branch: skipped")
	assert.Contains(t, out, "project symbols: stale")
	assert.Contains(t, out, "refresh skipped")
	assert.Contains(t, out, "previous fresh")
	assert.Contains(t, out, "text: StaleSymbol")
}

//nolint:paralleltest // Captures process stdout and uses process-wide prompt context source hooks.
func TestPromptComplete_RefreshesStaleContextSynchronously(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "fresh.go"), []byte("package fresh\n\nfunc FreshCliSymbol() {}\n"), 0o600))

	indexCalls := 0
	restoreIndex := setPromptCodeIndexDirContextForTest(func(_ context.Context, root string) (codeintel.Index, error) {
		indexCalls++

		return codeintel.Index{Symbols: []codeintel.Symbol{{
			Name: "FreshCliSymbol",
			Kind: "func",
			File: filepath.Join(root, "fresh.go"),
			Line: 3,
		}}}, nil
	})
	t.Cleanup(restoreIndex)

	cache := newPromptContextCache()
	state := appState{cwd: dir, promptContextCache: cache}
	key := promptContextCacheKeyForState(state)
	cache.store(promptRepoContextSnapshot{
		CapturedAt: time.Now().Add(-time.Minute),
		Key:        key,
		ProjectSymbols: []promptcomplete.Candidate{{
			Text:   "StaleCliSymbol",
			Kind:   "project-symbol",
			Source: "project symbol index",
		}},
		Sources: []promptContextSourceStatus{
			promptContextSourceReport(promptContextSourceProjectSymbols, promptContextFreshnessFresh, "1 candidate"),
		},
	})

	out := captureStdoutForStateDiagnostics(t, func() {
		promptComplete(context.Background(), state, "FreshCli", 1)
	})

	assert.Equal(t, 1, indexCalls)
	assert.Contains(t, out, "context:\n")
	assert.Contains(t, out, "project symbols: fresh")
	assert.Contains(t, out, "text: FreshCliSymbol")
	assert.NotContains(t, out, "text: StaleCliSymbol")
}

func TestFormatPromptContextSources_IncludesFreshnessDetails(t *testing.T) {
	t.Parallel()

	formatted := formatPromptContextSources([]promptContextSourceStatus{
		promptContextSourceReport(promptContextSourceProjectSymbols, promptContextFreshnessStale, "cached 2s ago"),
		promptContextSourceReport(promptContextSourceGitStatus, promptContextFreshnessPartial, "deadline exceeded"),
	})

	assert.Contains(t, formatted, "context:\n")
	assert.Contains(t, formatted, "project symbols: stale (cached 2s ago)")
	assert.Contains(t, formatted, "git status: partial (deadline exceeded)")
}

func TestPromptContextStatusLabel_PrioritizesErrorsOverRepoSkipped(t *testing.T) {
	t.Parallel()

	label := promptContextStatusLabel([]promptContextSourceStatus{
		promptContextSourceReport(promptContextSourceGitBranch, promptContextFreshnessSkipped, "no .git metadata"),
		promptContextSourceReport(promptContextSourceGitStatus, promptContextFreshnessSkipped, "no .git metadata"),
		promptContextSourceReport(promptContextSourceTasks, promptContextFreshnessError, "task list failed"),
	})

	assert.Equal(t, "error:task-list", label)
}

func TestPromptContextStatusLabel_TreatsFreshSymbolsAsFreshWithoutGit(t *testing.T) {
	t.Parallel()

	label := promptContextStatusLabel([]promptContextSourceStatus{
		promptContextSourceReport(promptContextSourceGitBranch, promptContextFreshnessSkipped, "no .git metadata"),
		promptContextSourceReport(promptContextSourceGitStatus, promptContextFreshnessSkipped, "no .git metadata"),
		promptContextSourceReport(promptContextSourceProjectSymbols, promptContextFreshnessFresh, "1 candidate"),
	})

	assert.Equal(t, "fresh", label)
}

func TestPromptContextStatusLabel_SurfacesSkippedProjectSymbols(t *testing.T) {
	t.Parallel()

	label := promptContextStatusLabel([]promptContextSourceStatus{
		promptContextSourceReport(promptContextSourceGitBranch, promptContextFreshnessFresh, "0 candidates"),
		promptContextSourceReport(promptContextSourceGitStatus, promptContextFreshnessFresh, "1 candidate"),
		promptContextSourceReport(promptContextSourceProjectSymbols, promptContextFreshnessSkipped, "go file count exceeds limit 2000"),
	})

	assert.Equal(t, "skipped:project-symbols", label)
}

func TestPromptSlashCommandCandidates_DeriveFromDescriptors(t *testing.T) {
	t.Parallel()

	candidates := promptSlashCommandCandidates()

	byText := make(map[string]promptcomplete.Candidate, len(candidates))
	for _, candidate := range candidates {
		require.Equal(t, "slash-command", candidate.Kind)
		require.Equal(t, "interactive slash commands", candidate.Source)
		byText[candidate.Text] = candidate
	}

	for _, command := range slashCommandDescriptors() {
		candidate, ok := byText["/"+command.Name]
		require.True(t, ok, "descriptor %q should produce a completion candidate", command.Name)
		assert.Equal(t, command.Summary, candidate.Description)
		assert.Equal(t, command.CompletionTokens, candidate.Tokens)

		for _, alias := range command.Aliases {
			aliasCandidate, aliasOK := byText["/"+alias]
			require.True(t, aliasOK, "descriptor alias %q should produce a completion candidate", alias)
			assert.Equal(t, command.Summary, aliasCandidate.Description)
			assert.Equal(t, command.CompletionTokens, aliasCandidate.Tokens)
		}
	}
}

func candidateTexts(candidates []promptcomplete.Candidate) []string {
	out := make([]string, 0, len(candidates))
	for _, candidate := range candidates {
		out = append(out, candidate.Text)
	}

	return out
}

func countCandidateText(candidates []promptcomplete.Candidate, text string) int {
	count := 0

	for _, candidate := range candidates {
		if candidate.Text == text {
			count++
		}
	}

	return count
}

func statusForSource(t *testing.T, statuses []promptContextSourceStatus, source string) promptContextSourceStatus {
	t.Helper()

	for _, status := range statuses {
		if status.Source == source {
			return status
		}
	}

	require.Failf(t, "missing source status", "source %q not found in %#v", source, statuses)

	return promptContextSourceStatus{}
}

func setPromptCodeIndexDirContextForTest(fn func(context.Context, string) (codeintel.Index, error)) func() {
	promptContextSourceFuncMu.Lock()
	previous := promptCodeIndexDirContext
	promptCodeIndexDirContext = fn
	promptContextSourceFuncMu.Unlock()

	return func() {
		promptContextSourceFuncMu.Lock()
		promptCodeIndexDirContext = previous
		promptContextSourceFuncMu.Unlock()
	}
}

func setPromptGitOutputFuncForTest(fn func(context.Context, string, ...string) (string, error)) func() {
	promptContextSourceFuncMu.Lock()
	previous := promptGitOutputFunc
	promptGitOutputFunc = fn
	promptContextSourceFuncMu.Unlock()

	return func() {
		promptContextSourceFuncMu.Lock()
		promptGitOutputFunc = previous
		promptContextSourceFuncMu.Unlock()
	}
}

func setPromptTaskCandidatesWithErrorForTest(
	fn func(context.Context, *session.Store, string, int) ([]promptcomplete.Candidate, error),
) func() {
	promptContextSourceFuncMu.Lock()
	previous := promptTaskCandidatesFunc
	promptTaskCandidatesFunc = fn
	promptContextSourceFuncMu.Unlock()

	return func() {
		promptContextSourceFuncMu.Lock()
		promptTaskCandidatesFunc = previous
		promptContextSourceFuncMu.Unlock()
	}
}
