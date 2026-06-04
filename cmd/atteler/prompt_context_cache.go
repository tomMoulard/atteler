package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/tommoulard/atteler/pkg/codeintel"
	"github.com/tommoulard/atteler/pkg/llm"
	"github.com/tommoulard/atteler/pkg/promptcomplete"
	"github.com/tommoulard/atteler/pkg/session"
	"github.com/tommoulard/atteler/pkg/tasklist"
)

const (
	promptContextSourceAgents         = "agents"
	promptContextSourceTools          = "tools"
	promptContextSourceTemplates      = "templates"
	promptContextSourceSlashCommands  = "slash commands"
	promptContextSourcePermissions    = "permissions"
	promptContextSourceSession        = "session state"
	promptContextSourceTasks          = "task list"
	promptContextSourceGitBranch      = "git branch"
	promptContextSourceGitStatus      = "git status"
	promptContextSourceProjectSymbols = "project symbols"

	promptGitRevisionNoGit                    = "no-git"
	promptContextHiddenSelectedAgentKeyPrefix = "hidden:"

	promptContextCacheVersion          = 1
	promptContextCacheFileName         = "prompt-context-cache.json"
	maxPromptContextCacheSnapshots     = 32
	defaultPromptContextCacheTTL       = 30 * time.Second
	defaultPromptContextSessionTimeout = 50 * time.Millisecond
	defaultPromptContextTaskTimeout    = 150 * time.Millisecond
	defaultPromptContextGitTimeout     = 200 * time.Millisecond
	defaultPromptContextIndexTimeout   = 2 * time.Second
	defaultPromptContextMaxIndexFiles  = 2000
	defaultPromptContextMaxSymbols     = 2000
	promptSessionRevisionTailItems     = 8
	promptSessionRevisionSampleBytes   = 512
)

type promptContextFreshness string

const (
	promptContextFreshnessFresh   promptContextFreshness = "fresh"
	promptContextFreshnessStale   promptContextFreshness = "stale"
	promptContextFreshnessPartial promptContextFreshness = "partial"
	promptContextFreshnessSkipped promptContextFreshness = "skipped"
	promptContextFreshnessError   promptContextFreshness = "error"
)

type promptContextSourceStatus struct {
	Source string                 `json:"source"`
	Status promptContextFreshness `json:"status"`
	Detail string                 `json:"detail"`
}

type promptCompletionContextResult struct {
	Context promptcomplete.Context
	Sources []promptContextSourceStatus
}

type promptCompletionContextOptions struct {
	Now            func() time.Time
	CacheTTL       time.Duration
	SessionTimeout time.Duration
	TaskTimeout    time.Duration
	GitTimeout     time.Duration
	IndexTimeout   time.Duration
	MaxCandidates  int
	MaxIndexFiles  int
	WaitForRepo    bool
}

type promptContextCacheKey struct {
	Root            string `json:"root"`
	GitRevision     string `json:"git_revision"`
	SelectedAgent   string `json:"selected_agent"`
	SessionRevision string `json:"session_revision"`
	TaskRevision    string `json:"task_revision"`
}

type promptRepoContextSnapshot struct {
	CapturedAt     time.Time                   `json:"captured_at"`
	Key            promptContextCacheKey       `json:"key"`
	ProjectSymbols []promptcomplete.Candidate  `json:"project_symbols"`
	RecentFiles    []promptcomplete.Candidate  `json:"recent_files"`
	Issues         []promptcomplete.Candidate  `json:"issues"`
	Sources        []promptContextSourceStatus `json:"sources"`
}

type promptContextCache struct {
	snapshots map[promptContextCacheKey]promptRepoContextSnapshot
	refreshes map[promptContextCacheKey]bool
	path      string
	mu        sync.Mutex
	saveMu    sync.Mutex
}

type promptContextCacheFile struct {
	Snapshots []promptRepoContextSnapshot `json:"snapshots"`
	Version   int                         `json:"version"`
}

type promptSourcePayload struct {
	ProjectSymbols []promptcomplete.Candidate
	RecentFiles    []promptcomplete.Candidate
	Issues         []promptcomplete.Candidate
	Tasks          []promptcomplete.Candidate
}

type promptSourceResult struct {
	err     error
	payload promptSourcePayload
}

type promptContextSkipError struct {
	reason string
}

type promptContextPartialError struct {
	reason          string
	outputTruncated bool
}

var (
	promptContextSourceFuncMu sync.RWMutex
	promptCodeIndexDirContext = codeintel.IndexDirContext
	promptGitOutputFunc       = promptGitOutput
	promptTaskCandidatesFunc  = loadPromptTaskCandidatesWithError
)

func runPromptCodeIndexDirContext(ctx context.Context, root string) (codeintel.Index, error) {
	promptContextSourceFuncMu.RLock()

	fn := promptCodeIndexDirContext

	promptContextSourceFuncMu.RUnlock()

	return fn(ctx, root)
}

func runPromptGitOutput(ctx context.Context, root string, args ...string) (string, error) {
	promptContextSourceFuncMu.RLock()

	fn := promptGitOutputFunc

	promptContextSourceFuncMu.RUnlock()

	return fn(ctx, root, args...)
}

func runPromptTaskCandidatesWithError(
	ctx context.Context,
	store *session.Store,
	root string,
	maxCandidates int,
) ([]promptcomplete.Candidate, error) {
	promptContextSourceFuncMu.RLock()

	fn := promptTaskCandidatesFunc

	promptContextSourceFuncMu.RUnlock()

	return fn(ctx, store, root, maxCandidates)
}

func (e promptContextSkipError) Error() string {
	return e.reason
}

func (e promptContextPartialError) Error() string {
	return e.reason
}

func newPromptContextCache(paths ...string) *promptContextCache {
	cache := &promptContextCache{
		snapshots: make(map[promptContextCacheKey]promptRepoContextSnapshot),
		refreshes: make(map[promptContextCacheKey]bool),
	}
	if len(paths) > 0 {
		cache.path = strings.TrimSpace(paths[0])
		cache.load()
	}

	return cache
}

func promptContextCachePath(store *session.Store) string {
	if store == nil || strings.TrimSpace(store.Dir()) == "" {
		return ""
	}

	return filepath.Join(filepath.Dir(store.Dir()), promptContextCacheFileName)
}

func (cache *promptContextCache) load() {
	if cache == nil || cache.path == "" {
		return
	}

	data, err := os.ReadFile(cache.path)
	if err != nil {
		return
	}

	var file promptContextCacheFile
	if err := json.Unmarshal(data, &file); err != nil || file.Version != promptContextCacheVersion { //nolint:musttag // Internal versioned cache file; promptcomplete.Candidate lacks JSON tags.
		return
	}

	for i := range file.Snapshots {
		snapshot := &file.Snapshots[i]
		if snapshot.Key.Root == "" {
			continue
		}

		cache.snapshots[snapshot.Key] = clonePromptRepoContextSnapshot(*snapshot)
	}

	cache.mu.Lock()
	cache.pruneLocked()
	cache.mu.Unlock()
}

func savePromptContextCacheFile(path string, snapshots []promptRepoContextSnapshot) {
	if path == "" {
		return
	}

	file := promptContextCacheFile{
		Version:   promptContextCacheVersion,
		Snapshots: snapshots,
	}

	data, err := json.MarshalIndent(file, "", "  ") //nolint:musttag // Internal versioned cache file; promptcomplete.Candidate lacks JSON tags.
	if err != nil {
		return
	}

	dir := filepath.Dir(path)
	if mkdirErr := os.MkdirAll(dir, 0o700); mkdirErr != nil {
		return
	}

	tmp, err := os.CreateTemp(dir, ".prompt-context-cache-*.tmp")
	if err != nil {
		return
	}

	tmpPath := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpPath)

		return
	}

	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpPath)

		return
	}

	if err := os.Rename(tmpPath, path); err != nil {
		_ = os.Remove(tmpPath)
	}
}

func (cache *promptContextCache) snapshotsForSaveLocked() []promptRepoContextSnapshot {
	snapshots := make([]promptRepoContextSnapshot, 0, len(cache.snapshots))
	for _, snapshot := range cache.snapshots { //nolint:gocritic // Snapshot copies keep persisted values isolated from map mutation.
		snapshots = append(snapshots, clonePromptRepoContextSnapshot(snapshot))
	}

	sort.Slice(snapshots, func(i, j int) bool {
		return snapshots[i].CapturedAt.After(snapshots[j].CapturedAt)
	})

	if len(snapshots) > maxPromptContextCacheSnapshots {
		snapshots = snapshots[:maxPromptContextCacheSnapshots]
	}

	return snapshots
}

func defaultPromptCompletionContextOptions() promptCompletionContextOptions {
	return promptCompletionContextOptions{
		CacheTTL:       defaultPromptContextCacheTTL,
		SessionTimeout: defaultPromptContextSessionTimeout,
		TaskTimeout:    defaultPromptContextTaskTimeout,
		GitTimeout:     defaultPromptContextGitTimeout,
		IndexTimeout:   defaultPromptContextIndexTimeout,
		MaxCandidates:  maxPromptContextCandidates,
		MaxIndexFiles:  defaultPromptContextMaxIndexFiles,
		WaitForRepo:    true,
	}
}

func promptCompletionContextWithFreshness(ctx context.Context, state appState, input string, includeRepo bool) promptCompletionContextResult {
	return promptCompletionContextWithOptions(ctx, state, input, includeRepo, defaultPromptCompletionContextOptions())
}

func promptCompletionContextInteractive(ctx context.Context, state appState, input string, includeRepo bool) promptCompletionContextResult {
	opts := defaultPromptCompletionContextOptions()
	opts.WaitForRepo = false

	return promptCompletionContextWithOptions(ctx, state, input, includeRepo, opts)
}

func promptCompletionContextWithOptions(
	ctx context.Context,
	state appState,
	input string,
	includeRepo bool,
	opts promptCompletionContextOptions,
) promptCompletionContextResult {
	opts = normalizePromptCompletionContextOptions(opts)

	completionContext := promptcomplete.Context{
		Input:         input,
		Cursor:        len(input),
		Agents:        limitPromptCandidates(promptAgentCandidates(state.agentRegistry), opts.MaxCandidates),
		Tools:         limitPromptCandidates(promptToolCandidates(), opts.MaxCandidates),
		Templates:     limitPromptCandidates(promptTemplateCandidates(), opts.MaxCandidates),
		SlashCommands: limitPromptCandidates(promptSlashCommandCandidates(), opts.MaxCandidates),
		Permissions:   limitPromptCandidates(promptPermissionCandidates(state.agentRegistry, state.selectedAgent), opts.MaxCandidates),
	}

	statuses := []promptContextSourceStatus{
		promptContextSourceReport(promptContextSourceAgents, promptContextFreshnessFresh, candidateCountDetail(len(completionContext.Agents))),
		promptContextSourceReport(promptContextSourceTools, promptContextFreshnessFresh, candidateCountDetail(len(completionContext.Tools))),
		promptContextSourceReport(promptContextSourceTemplates, promptContextFreshnessFresh, candidateCountDetail(len(completionContext.Templates))),
		promptContextSourceReport(promptContextSourceSlashCommands, promptContextFreshnessFresh, candidateCountDetail(len(completionContext.SlashCommands))),
		promptContextSourceReport(promptContextSourcePermissions, promptContextFreshnessFresh, candidateCountDetail(len(completionContext.Permissions))),
	}

	sessionPayload, sessionStatus := promptSessionSource(ctx, state.sessionState, opts)
	completionContext.RecentFiles = append(completionContext.RecentFiles, sessionPayload.RecentFiles...)
	completionContext.Issues = append(completionContext.Issues, sessionPayload.Issues...)
	statuses = append(statuses, sessionStatus)

	taskPayload, taskStatus := promptTaskSource(ctx, state.sessionStore, promptContextRootForState(state), opts)
	completionContext.Tasks = append(completionContext.Tasks, taskPayload.Tasks...)
	completionContext.Issues = append(completionContext.Issues, limitPromptCandidates(promptIssueCandidatesFromTasks(taskPayload.Tasks), opts.MaxCandidates)...)
	statuses = append(statuses, taskStatus)

	if includeRepo {
		repo := promptRepoSnapshotForState(ctx, state, opts)
		completionContext.ProjectSymbols = append(
			completionContext.ProjectSymbols,
			filterPromptProjectSymbols(repo.ProjectSymbols, input, opts.MaxCandidates)...,
		)
		completionContext.RecentFiles = append(completionContext.RecentFiles, limitPromptCandidates(repo.RecentFiles, opts.MaxCandidates)...)
		completionContext.Issues = append(completionContext.Issues, limitPromptCandidates(repo.Issues, opts.MaxCandidates)...)
		statuses = append(statuses, repo.Sources...)
	} else {
		statuses = append(statuses,
			promptContextSourceReport(promptContextSourceGitBranch, promptContextFreshnessSkipped, "repo context disabled"),
			promptContextSourceReport(promptContextSourceGitStatus, promptContextFreshnessSkipped, "repo context disabled"),
			promptContextSourceReport(promptContextSourceProjectSymbols, promptContextFreshnessSkipped, "repo context disabled"),
		)
	}

	completionContext.Issues = limitPromptCandidates(dedupePromptCandidates(completionContext.Issues), opts.MaxCandidates)
	completionContext.RecentFiles = limitPromptCandidates(dedupePromptCandidates(completionContext.RecentFiles), opts.MaxCandidates)

	return promptCompletionContextResult{Context: completionContext, Sources: statuses}
}

func normalizePromptCompletionContextOptions(opts promptCompletionContextOptions) promptCompletionContextOptions {
	if opts.Now == nil {
		opts.Now = time.Now
	}

	if opts.CacheTTL <= 0 {
		opts.CacheTTL = defaultPromptContextCacheTTL
	}

	if opts.SessionTimeout <= 0 {
		opts.SessionTimeout = defaultPromptContextSessionTimeout
	}

	if opts.TaskTimeout <= 0 {
		opts.TaskTimeout = defaultPromptContextTaskTimeout
	}

	if opts.GitTimeout <= 0 {
		opts.GitTimeout = defaultPromptContextGitTimeout
	}

	if opts.IndexTimeout <= 0 {
		opts.IndexTimeout = defaultPromptContextIndexTimeout
	}

	if opts.MaxCandidates <= 0 {
		opts.MaxCandidates = maxPromptContextCandidates
	}

	if opts.MaxIndexFiles <= 0 {
		opts.MaxIndexFiles = defaultPromptContextMaxIndexFiles
	}

	return opts
}

func promptSessionSource(ctx context.Context, sessionState session.Session, opts promptCompletionContextOptions) (promptSourcePayload, promptContextSourceStatus) {
	return runPromptSource(ctx, opts.SessionTimeout, promptContextSourceSession, func(context.Context) (promptSourcePayload, error) {
		files := promptSessionFileCandidates(sessionState, opts.MaxCandidates)
		issues := promptIssueCandidatesFromSession(sessionState, opts.MaxCandidates)

		return promptSourcePayload{RecentFiles: files, Issues: issues}, nil
	})
}

func promptTaskSource(
	ctx context.Context,
	store *session.Store,
	root string,
	opts promptCompletionContextOptions,
) (promptSourcePayload, promptContextSourceStatus) {
	return runPromptSource(ctx, opts.TaskTimeout, promptContextSourceTasks, func(ctx context.Context) (promptSourcePayload, error) {
		taskList := taskListPath(store, "")
		if _, err := os.Stat(taskList); err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return promptSourcePayload{}, promptContextSkipError{reason: "task list not found"}
			}

			return promptSourcePayload{}, fmt.Errorf("stat prompt tasks: %w", err)
		}

		tasks, err := runPromptTaskCandidatesWithError(ctx, store, root, opts.MaxCandidates)
		if err != nil {
			return promptSourcePayload{}, err
		}

		return promptSourcePayload{Tasks: tasks}, nil
	})
}

func promptRepoSnapshotForState(ctx context.Context, state appState, opts promptCompletionContextOptions) promptRepoContextSnapshot {
	key := promptContextCacheKeyForState(state)
	ctx = contextWithPromptGitAutonomy(ctx, state.autonomy)

	if !promptGitCompletionAllowed(state.autonomy) {
		return buildPromptRepoContextSnapshot(ctx, key, opts)
	}

	cache := state.promptContextCache
	if cache == nil {
		cache = newPromptContextCache()
	}

	return cache.snapshot(ctx, key, opts)
}

func (cache *promptContextCache) snapshot(
	ctx context.Context,
	key promptContextCacheKey,
	opts promptCompletionContextOptions,
) promptRepoContextSnapshot {
	if cache == nil {
		return buildPromptRepoContextSnapshot(ctx, key, opts)
	}

	now := opts.Now()
	if snapshot, ok := cache.freshSnapshot(key, now, opts.CacheTTL); ok {
		return snapshot
	}

	if stale, ok := cache.compatibleSnapshot(key); ok {
		if opts.WaitForRepo {
			snapshot := buildPromptRepoContextSnapshot(ctx, key, opts)
			snapshot = preserveFallbackPromptRepoSources(snapshot, stale, now)
			cache.store(snapshot)

			return snapshot
		}

		cache.refreshAsync(ctx, key, opts, stale)

		return stalePromptRepoContextSnapshot(stale, key, now)
	}

	if !opts.WaitForRepo && (ctx == nil || strings.TrimSpace(key.Root) == "") {
		return buildPromptRepoContextSnapshot(ctx, key, opts)
	}

	if !opts.WaitForRepo {
		cache.refreshAsync(ctx, key, opts, promptRepoContextSnapshot{})

		return promptRepoContextSnapshot{
			CapturedAt: now,
			Key:        key,
			Sources: []promptContextSourceStatus{
				promptContextSourceReport(promptContextSourceGitBranch, promptContextFreshnessSkipped, "warming prompt context cache"),
				promptContextSourceReport(promptContextSourceGitStatus, promptContextFreshnessSkipped, "warming prompt context cache"),
				promptContextSourceReport(promptContextSourceProjectSymbols, promptContextFreshnessSkipped, "warming prompt context cache"),
			},
		}
	}

	snapshot := buildPromptRepoContextSnapshot(ctx, key, opts)
	cache.store(snapshot)

	return snapshot
}

func (cache *promptContextCache) freshSnapshot(
	key promptContextCacheKey,
	now time.Time,
	ttl time.Duration,
) (promptRepoContextSnapshot, bool) {
	cache.mu.Lock()
	defer cache.mu.Unlock()

	snapshot, ok := cache.snapshots[key]
	if !ok || now.Sub(snapshot.CapturedAt) > ttl {
		return promptRepoContextSnapshot{}, false
	}

	return clonePromptRepoContextSnapshot(snapshot), true
}

func (cache *promptContextCache) compatibleSnapshot(key promptContextCacheKey) (promptRepoContextSnapshot, bool) {
	if strings.TrimSpace(key.Root) == "" {
		return promptRepoContextSnapshot{}, false
	}

	cache.mu.Lock()
	defer cache.mu.Unlock()

	var (
		best   promptRepoContextSnapshot
		found  bool
		bestAt time.Time
	)

	for cachedKey, snapshot := range cache.snapshots { //nolint:gocritic // Snapshot copies keep callers isolated from cache mutation.
		if cachedKey.Root != key.Root || cachedKey.SelectedAgent != key.SelectedAgent {
			continue
		}

		if found && !snapshot.CapturedAt.After(bestAt) {
			continue
		}

		best = snapshot
		bestAt = snapshot.CapturedAt
		found = true
	}

	if !found {
		return promptRepoContextSnapshot{}, false
	}

	return clonePromptRepoContextSnapshot(best), true
}

func (cache *promptContextCache) refreshAsync(
	ctx context.Context,
	key promptContextCacheKey,
	opts promptCompletionContextOptions,
	fallback promptRepoContextSnapshot,
) {
	cache.mu.Lock()
	if cache.refreshes[key] {
		cache.mu.Unlock()

		return
	}

	cache.refreshes[key] = true
	cache.mu.Unlock()

	go func() {
		defer func() {
			cache.mu.Lock()
			delete(cache.refreshes, key)
			cache.mu.Unlock()
		}()

		snapshot := buildPromptRepoContextSnapshot(ctx, key, opts)
		snapshot = preserveFallbackPromptRepoSources(snapshot, fallback, opts.Now())
		cache.store(snapshot)
	}()
}

func preserveFallbackPromptRepoSources(
	snapshot promptRepoContextSnapshot,
	fallback promptRepoContextSnapshot,
	now time.Time,
) promptRepoContextSnapshot {
	if strings.TrimSpace(fallback.Key.Root) == "" ||
		fallback.Key.Root != snapshot.Key.Root ||
		fallback.Key.SelectedAgent != snapshot.Key.SelectedAgent {
		return snapshot
	}

	age := now.Sub(fallback.CapturedAt).Round(time.Millisecond)
	age = max(age, 0)

	fallbackSources := make(map[string]promptContextSourceStatus, len(fallback.Sources))
	for _, status := range fallback.Sources {
		fallbackSources[status.Source] = status
	}

	for i := range snapshot.Sources {
		status := snapshot.Sources[i]
		if status.Status == promptContextFreshnessFresh || !fallbackHasPromptSourceCandidates(fallback, status.Source) {
			continue
		}

		snapshot = replacePromptSourceCandidates(snapshot, fallback, status.Source)
		snapshot.Sources[i] = promptContextSourceReport(
			status.Source,
			promptContextFreshnessStale,
			fallbackRefreshDetail(age, fallbackSources[status.Source], status),
		)
	}

	return snapshot
}

func fallbackHasPromptSourceCandidates(snapshot promptRepoContextSnapshot, source string) bool {
	switch source {
	case promptContextSourceGitBranch:
		return len(snapshot.Issues) > 0
	case promptContextSourceGitStatus:
		return len(snapshot.RecentFiles) > 0
	case promptContextSourceProjectSymbols:
		return len(snapshot.ProjectSymbols) > 0
	default:
		return false
	}
}

func replacePromptSourceCandidates(
	snapshot promptRepoContextSnapshot,
	fallback promptRepoContextSnapshot,
	source string,
) promptRepoContextSnapshot {
	switch source {
	case promptContextSourceGitBranch:
		snapshot.Issues = clonePromptCandidates(fallback.Issues)
	case promptContextSourceGitStatus:
		snapshot.RecentFiles = clonePromptCandidates(fallback.RecentFiles)
	case promptContextSourceProjectSymbols:
		snapshot.ProjectSymbols = clonePromptCandidates(fallback.ProjectSymbols)
	}

	return snapshot
}

func fallbackRefreshDetail(age time.Duration, previous, refresh promptContextSourceStatus) string {
	detail := "cached " + age.String() + " ago; refresh " + string(refresh.Status)
	if refresh.Detail != "" {
		detail += ": " + refresh.Detail
	}

	if previous.Status != "" {
		detail += "; previous " + string(previous.Status)
	}

	if previous.Detail != "" {
		detail += ": " + previous.Detail
	}

	return detail
}

func (cache *promptContextCache) store(snapshot promptRepoContextSnapshot) {
	if cache == nil {
		return
	}

	if strings.TrimSpace(snapshot.Key.Root) == "" {
		return
	}

	cache.mu.Lock()
	cache.snapshots[snapshot.Key] = clonePromptRepoContextSnapshot(snapshot)
	cache.pruneLocked()
	path := cache.path
	cache.mu.Unlock()

	cache.save(path)
}

func (cache *promptContextCache) save(path string) {
	if path == "" {
		return
	}

	cache.saveMu.Lock()
	defer cache.saveMu.Unlock()

	cache.mu.Lock()
	snapshots := cache.snapshotsForSaveLocked()
	cache.mu.Unlock()

	savePromptContextCacheFile(path, snapshots)
}

func (cache *promptContextCache) pruneLocked() {
	if len(cache.snapshots) <= maxPromptContextCacheSnapshots {
		return
	}

	//nolint:govet // Keeps cache key before ordering metadata for readability.
	type snapshotKeyTime struct {
		key        promptContextCacheKey
		capturedAt time.Time
	}

	keys := make([]snapshotKeyTime, 0, len(cache.snapshots))
	for key, snapshot := range cache.snapshots { //nolint:gocritic // Snapshot timestamp is the value needed to prune by recency.
		keys = append(keys, snapshotKeyTime{key: key, capturedAt: snapshot.CapturedAt})
	}

	sort.Slice(keys, func(i, j int) bool {
		return keys[i].capturedAt.After(keys[j].capturedAt)
	})

	for _, entry := range keys[maxPromptContextCacheSnapshots:] {
		delete(cache.snapshots, entry.key)
	}
}

func buildPromptRepoContextSnapshot(
	ctx context.Context,
	key promptContextCacheKey,
	opts promptCompletionContextOptions,
) promptRepoContextSnapshot {
	snapshot := promptRepoContextSnapshot{
		CapturedAt: opts.Now(),
		Key:        key,
	}
	if ctx == nil {
		snapshot.Sources = []promptContextSourceStatus{
			promptContextSourceReport(promptContextSourceGitBranch, promptContextFreshnessSkipped, "context is empty"),
			promptContextSourceReport(promptContextSourceGitStatus, promptContextFreshnessSkipped, "context is empty"),
			promptContextSourceReport(promptContextSourceProjectSymbols, promptContextFreshnessSkipped, "context is empty"),
		}

		return snapshot
	}

	if strings.TrimSpace(key.Root) == "" {
		snapshot.Sources = []promptContextSourceStatus{
			promptContextSourceReport(promptContextSourceGitBranch, promptContextFreshnessSkipped, "repository root is empty"),
			promptContextSourceReport(promptContextSourceGitStatus, promptContextFreshnessSkipped, "repository root is empty"),
			promptContextSourceReport(promptContextSourceProjectSymbols, promptContextFreshnessSkipped, "repository root is empty"),
		}

		return snapshot
	}

	//nolint:govet // Result shape mirrors the source payload/status pairing.
	type repoSourceResult struct {
		payload promptSourcePayload
		status  promptContextSourceStatus
	}

	type repoSourceJob struct {
		run func() repoSourceResult
	}

	jobs := []repoSourceJob{}
	gitAllowed := promptGitCompletionAllowed(promptGitAutonomyFromContext(ctx))

	switch {
	case key.GitRevision == promptGitRevisionNoGit:
		snapshot.Sources = append(snapshot.Sources,
			promptContextSourceReport(promptContextSourceGitBranch, promptContextFreshnessSkipped, "no .git metadata"),
			promptContextSourceReport(promptContextSourceGitStatus, promptContextFreshnessSkipped, "no .git metadata"),
		)
	case !gitAllowed:
		snapshot.Sources = append(snapshot.Sources,
			promptContextSourceReport(promptContextSourceGitBranch, promptContextFreshnessSkipped, "autonomy low skips git completion probes"),
			promptContextSourceReport(promptContextSourceGitStatus, promptContextFreshnessSkipped, "autonomy low skips git completion probes"),
		)
	default:
		jobs = append(jobs,
			repoSourceJob{
				run: func() repoSourceResult {
					payload, status := runPromptSource(ctx, opts.GitTimeout, promptContextSourceGitBranch, func(ctx context.Context) (promptSourcePayload, error) {
						issues, err := promptGitIssueCandidatesWithError(ctx, key.Root, opts.MaxCandidates)
						if err != nil {
							return promptSourcePayload{Issues: issues}, err
						}

						return promptSourcePayload{Issues: issues}, nil
					})

					return repoSourceResult{payload: payload, status: status}
				},
			},
			repoSourceJob{
				run: func() repoSourceResult {
					payload, status := runPromptSource(ctx, opts.GitTimeout, promptContextSourceGitStatus, func(ctx context.Context) (promptSourcePayload, error) {
						files, err := promptGitRecentFileCandidatesWithError(ctx, key.Root, opts.MaxCandidates)
						if err != nil {
							return promptSourcePayload{RecentFiles: files}, err
						}

						return promptSourcePayload{RecentFiles: files}, nil
					})

					return repoSourceResult{payload: payload, status: status}
				},
			},
		)
	}

	jobs = append(jobs, repoSourceJob{
		run: func() repoSourceResult {
			payload, status := runPromptSource(ctx, opts.IndexTimeout, promptContextSourceProjectSymbols, func(ctx context.Context) (promptSourcePayload, error) {
				symbolLimit := max(opts.MaxCandidates, defaultPromptContextMaxSymbols)

				symbols, err := promptProjectSymbolCandidatesWithError(ctx, key.Root, "", symbolLimit, opts.MaxIndexFiles)
				if err != nil {
					return promptSourcePayload{}, err
				}

				return promptSourcePayload{ProjectSymbols: symbols}, nil
			})

			return repoSourceResult{payload: payload, status: status}
		},
	})

	results := make(chan repoSourceResult, len(jobs))
	for _, job := range jobs {
		go func(job repoSourceJob) {
			results <- job.run()
		}(job)
	}

	byName := make(map[string]repoSourceResult, len(jobs))
	for range jobs {
		result := <-results
		byName[result.status.Source] = result
	}

	for _, source := range []string{promptContextSourceGitBranch, promptContextSourceGitStatus, promptContextSourceProjectSymbols} {
		result, ok := byName[source]
		if !ok {
			continue
		}

		snapshot.ProjectSymbols = append(snapshot.ProjectSymbols, result.payload.ProjectSymbols...)
		snapshot.RecentFiles = append(snapshot.RecentFiles, result.payload.RecentFiles...)
		snapshot.Issues = append(snapshot.Issues, result.payload.Issues...)
		snapshot.Sources = append(snapshot.Sources, result.status)
	}

	return snapshot
}

func runPromptSource(
	ctx context.Context,
	timeout time.Duration,
	source string,
	fn func(context.Context) (promptSourcePayload, error),
) (promptSourcePayload, promptContextSourceStatus) {
	if ctx == nil {
		return promptSourcePayload{}, promptContextSourceReport(source, promptContextFreshnessSkipped, "context is empty")
	}

	sourceCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	ch := make(chan promptSourceResult, 1)

	go func() {
		payload, err := fn(sourceCtx)
		ch <- promptSourceResult{payload: payload, err: err}
	}()

	select {
	case result := <-ch:
		if result.err != nil {
			var partialErr promptContextPartialError
			if errors.As(result.err, &partialErr) {
				return result.payload, promptContextSourceReport(source, promptContextFreshnessPartial, partialErr.reason)
			}

			var skipErr promptContextSkipError
			if errors.As(result.err, &skipErr) {
				return promptSourcePayload{}, promptContextSourceReport(source, promptContextFreshnessSkipped, skipErr.reason)
			}

			if errors.Is(sourceCtx.Err(), context.DeadlineExceeded) || errors.Is(result.err, context.DeadlineExceeded) {
				return promptSourcePayload{}, promptContextSourceReport(source, promptContextFreshnessPartial, "deadline exceeded after "+timeout.String())
			}

			return promptSourcePayload{}, promptContextSourceReport(source, promptContextFreshnessError, result.err.Error())
		}

		return result.payload, promptContextSourceReport(source, promptContextFreshnessFresh, candidateCountDetail(promptSourcePayloadCandidateCount(result.payload)))
	case <-sourceCtx.Done():
		if errors.Is(sourceCtx.Err(), context.DeadlineExceeded) {
			return promptSourcePayload{}, promptContextSourceReport(source, promptContextFreshnessPartial, "deadline exceeded after "+timeout.String())
		}

		return promptSourcePayload{}, promptContextSourceReport(source, promptContextFreshnessError, sourceCtx.Err().Error())
	}
}

func stalePromptRepoContextSnapshot(
	snapshot promptRepoContextSnapshot,
	requestedKey promptContextCacheKey,
	now time.Time,
) promptRepoContextSnapshot {
	age := now.Sub(snapshot.CapturedAt).Round(time.Millisecond)
	age = max(age, 0)

	staleSources := make([]promptContextSourceStatus, 0, len(snapshot.Sources))
	for _, source := range snapshot.Sources {
		status := promptContextFreshnessStale

		if !fallbackHasPromptSourceCandidates(snapshot, source.Source) {
			switch source.Status {
			case promptContextFreshnessError, promptContextFreshnessPartial, promptContextFreshnessSkipped:
				status = source.Status
			}
		}

		detail := "cached " + age.String() + " ago; refreshing"
		if source.Status != "" {
			detail += "; previous " + string(source.Status)
		}

		if source.Detail != "" {
			detail += ": " + source.Detail
		}

		staleSources = append(staleSources, promptContextSourceReport(source.Source, status, detail))
	}

	if len(staleSources) == 0 {
		staleSources = []promptContextSourceStatus{
			promptContextSourceReport(promptContextSourceGitBranch, promptContextFreshnessStale, "cached "+age.String()+" ago; refreshing"),
			promptContextSourceReport(promptContextSourceGitStatus, promptContextFreshnessStale, "cached "+age.String()+" ago; refreshing"),
			promptContextSourceReport(promptContextSourceProjectSymbols, promptContextFreshnessStale, "cached "+age.String()+" ago; refreshing"),
		}
	}

	snapshot.Key = requestedKey
	snapshot.Sources = staleSources

	return snapshot
}

func promptContextCacheKeyForState(state appState) promptContextCacheKey {
	root := promptContextRootForState(state)
	if root != "" {
		root = promptRepositoryRoot(root)
	}

	gitRevision := promptGitRevisionNoGit
	if root != "" {
		gitRevision = promptGitRevision(root)
	}

	return promptContextCacheKey{
		Root:            root,
		GitRevision:     gitRevision,
		SelectedAgent:   promptContextSelectedAgentKey(state),
		SessionRevision: promptSessionRevision(state.sessionState),
		TaskRevision:    promptTaskRevision(state.sessionStore),
	}
}

func promptContextRootForState(state appState) string {
	root := cleanPromptContextRoot(state.contextOptions.Root)
	cwd := cleanPromptContextRoot(state.cwd)

	if (root == "" || root == cwd) && state.worktreeInfo != nil {
		if worktreeRoot := cleanPromptContextRoot(state.worktreeInfo.Path); worktreeRoot != "" {
			root = worktreeRoot
		}
	}

	if root == "" {
		root = cwd
	}

	return root
}

func promptContextSelectedAgentKey(state appState) string {
	selectedAgent := strings.TrimSpace(state.selectedAgent)
	if selectedAgent == "" || state.agentRegistry == nil {
		return selectedAgent
	}

	activeAgent, ok := state.agentRegistry.Get(selectedAgent)
	if !ok || !activeAgent.Hidden {
		return selectedAgent
	}

	return promptContextHiddenSelectedAgentKey(selectedAgent)
}

func promptContextHiddenSelectedAgentKey(selectedAgent string) string {
	sum := sha256.Sum256([]byte(selectedAgent))

	return promptContextHiddenSelectedAgentKeyPrefix + hex.EncodeToString(sum[:])
}

func promptSessionRevision(sessionState session.Session) string {
	h := sha256.New()
	writeHashString(h, sessionState.ID)
	writeHashString(h, sessionState.Title)
	writeHashString(h, sessionState.DefaultAgent)
	writeHashString(h, sessionState.UpdatedAt.UTC().Format(time.RFC3339Nano))
	writeHashString(h, fmt.Sprintf("tags:%d", len(sessionState.Tags)))
	writeHashString(h, fmt.Sprintf("messages:%d", len(sessionState.Messages)))
	writeHashString(h, fmt.Sprintf("artifacts:%d", len(sessionState.Artifacts)))

	for _, tag := range tailPromptSessionStrings(sessionState.Tags, promptSessionRevisionTailItems) {
		writeHashSampledString(h, tag)
	}

	for _, message := range tailPromptSessionMessages(sessionState.Messages, promptSessionRevisionTailItems) {
		writeHashString(h, string(message.Role))
		writeHashSampledString(h, message.Content)
	}

	artifacts := tailPromptSessionArtifacts(sessionState.Artifacts, promptSessionRevisionTailItems)
	for i := range artifacts {
		artifact := &artifacts[i]

		writeHashSampledString(h, artifact.Path)
		writeHashString(h, artifact.Kind)
		writeHashSampledString(h, artifact.Summary)
		writeHashString(h, artifact.SourceAgent)
		writeHashString(h, artifact.CreatedAt.UTC().Format(time.RFC3339Nano))
	}

	return hex.EncodeToString(h.Sum(nil))
}

func tailPromptSessionMessages(messages []llm.Message, limit int) []llm.Message {
	if limit <= 0 || len(messages) <= limit {
		return messages
	}

	return messages[len(messages)-limit:]
}

func tailPromptSessionArtifacts(artifacts []session.Artifact, limit int) []session.Artifact {
	if limit <= 0 || len(artifacts) <= limit {
		return artifacts
	}

	return artifacts[len(artifacts)-limit:]
}

func tailPromptSessionStrings(values []string, limit int) []string {
	if limit <= 0 || len(values) <= limit {
		return values
	}

	return values[len(values)-limit:]
}

func promptTaskRevision(store *session.Store) string {
	taskList := taskListPath(store, "")

	path, err := filepath.Abs(taskList)
	if err != nil {
		path = filepath.Clean(taskList)
	}

	info, err := os.Stat(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "missing:" + filepath.ToSlash(path)
		}

		return "error:" + filepath.ToSlash(path) + ":" + err.Error()
	}

	return fmt.Sprintf(
		"%s:%d:%d",
		filepath.ToSlash(path),
		info.Size(),
		info.ModTime().UTC().UnixNano(),
	)
}

func promptGitRevision(root string) string {
	gitDir, ok := promptGitDir(root)
	if !ok {
		return promptGitRevisionNoGit
	}

	commonDir := promptGitCommonDir(gitDir)

	h := sha256.New()
	writeHashString(h, filepath.ToSlash(gitDir))
	writeHashString(h, filepath.ToSlash(commonDir))
	writePromptGitHeadRevision(h, gitDir, commonDir)
	writePromptGitPackedRefsRevision(h, commonDir)
	writePromptGitIndexRevision(h, gitDir)

	return hex.EncodeToString(h.Sum(nil))
}

func promptRepositoryRoot(start string) string {
	root := cleanPromptContextRoot(start)
	if root == "" {
		return ""
	}

	for dir := root; ; dir = filepath.Dir(dir) {
		candidate := filepath.Join(dir, ".git")

		info, err := os.Stat(candidate)
		if err == nil {
			if info.IsDir() {
				return dir
			}

			if _, ok := promptGitDirFromFile(dir, candidate); ok {
				return dir
			}
		}

		parent := filepath.Dir(dir)
		if parent == dir {
			return root
		}
	}
}

func writePromptGitHeadRevision(h hash.Hash, gitDir, commonDir string) {
	headPath := filepath.Join(gitDir, "HEAD")

	head, err := os.ReadFile(headPath)
	if err != nil {
		writeHashString(h, "head-error:"+err.Error())

		return
	}

	headText := strings.TrimSpace(string(head))
	writeHashString(h, "head:"+headText)

	ref, ok := strings.CutPrefix(headText, "ref: ")
	if !ok {
		return
	}

	writePromptGitRefRevision(h, gitDir, commonDir, ref)
}

func writePromptGitPackedRefsRevision(h hash.Hash, commonDir string) {
	info, err := os.Stat(filepath.Join(commonDir, "packed-refs"))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			writeHashString(h, "packed-refs-missing")
		} else {
			writeHashString(h, "packed-refs-error:"+err.Error())
		}

		return
	}

	writeHashString(h, fmt.Sprintf("packed-refs:%d:%d", info.Size(), info.ModTime().UTC().UnixNano()))
}

func writePromptGitRefRevision(h hash.Hash, gitDir, commonDir, ref string) {
	refPaths, ok := promptGitRefPaths(gitDir, commonDir, ref)
	if !ok {
		writeHashString(h, "ref-invalid:"+strings.TrimSpace(ref))

		return
	}

	var lastErr error

	for _, refPath := range refPaths {
		refData, err := os.ReadFile(refPath)
		if err == nil {
			writeHashString(h, "ref:"+strings.TrimSpace(string(refData)))

			return
		}

		lastErr = err
	}

	if lastErr != nil {
		writeHashString(h, "ref-error:"+lastErr.Error())
	}
}

func promptGitRefPaths(gitDir, commonDir, ref string) ([]string, bool) {
	cleanRef := filepath.Clean(filepath.FromSlash(strings.TrimSpace(ref)))
	if cleanRef == "." || filepath.IsAbs(cleanRef) || cleanRef == ".." || strings.HasPrefix(cleanRef, ".."+string(os.PathSeparator)) {
		return nil, false
	}

	paths := []string{filepath.Join(gitDir, cleanRef)}
	if commonDir != "" && filepath.Clean(commonDir) != filepath.Clean(gitDir) {
		paths = append(paths, filepath.Join(commonDir, cleanRef))
	}

	return paths, true
}

func promptGitCommonDir(gitDir string) string {
	commonDirPath := filepath.Join(gitDir, "commondir")

	data, err := os.ReadFile(commonDirPath)
	if err != nil {
		return gitDir
	}

	commonDir := strings.TrimSpace(string(data))
	if commonDir == "" {
		return gitDir
	}

	if !filepath.IsAbs(commonDir) {
		commonDir = filepath.Join(gitDir, commonDir)
	}

	return filepath.Clean(commonDir)
}

func writePromptGitIndexRevision(h hash.Hash, gitDir string) {
	indexPath := filepath.Join(gitDir, "index")
	if info, err := os.Stat(indexPath); err == nil {
		writeHashString(h, fmt.Sprintf("index:%d:%d", info.Size(), info.ModTime().UTC().UnixNano()))
	} else if !errors.Is(err, os.ErrNotExist) {
		writeHashString(h, "index-error:"+err.Error())
	} else {
		writeHashString(h, "index-missing")
	}
}

func promptGitDir(root string) (string, bool) {
	root = cleanPromptContextRoot(root)
	if root == "" {
		return "", false
	}

	for dir := root; ; dir = filepath.Dir(dir) {
		candidate := filepath.Join(dir, ".git")

		info, err := os.Stat(candidate)
		if err == nil {
			if info.IsDir() {
				return candidate, true
			}

			if gitDir, ok := promptGitDirFromFile(dir, candidate); ok {
				return gitDir, true
			}
		}

		parent := filepath.Dir(dir)
		if parent == dir {
			return "", false
		}
	}
}

func promptGitDirFromFile(root, path string) (string, bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", false
	}

	line := strings.TrimSpace(string(data))

	gitDir, ok := strings.CutPrefix(line, "gitdir:")
	if !ok {
		return "", false
	}

	gitDir = strings.TrimSpace(gitDir)
	if gitDir == "" {
		return "", false
	}

	if !filepath.IsAbs(gitDir) {
		gitDir = filepath.Join(root, gitDir)
	}

	return filepath.Clean(gitDir), true
}

func promptProjectSymbolCandidatesWithError(
	ctx context.Context,
	root string,
	input string,
	maxCandidates int,
	maxIndexFiles int,
) ([]promptcomplete.Candidate, error) {
	maxCandidates = promptContextCandidateLimit(maxCandidates)

	root = strings.TrimSpace(root)
	if root == "" {
		return nil, promptContextSkipError{reason: "repository root is empty"}
	}

	goFiles, tooMany, err := countPromptContextGoFiles(ctx, root, maxIndexFiles)
	if err != nil {
		return nil, err
	}

	if tooMany {
		return nil, promptContextSkipError{reason: fmt.Sprintf("go file count exceeds limit %d", maxIndexFiles)}
	}

	if goFiles == 0 {
		return nil, promptContextSkipError{reason: "no Go files found"}
	}

	index, err := runPromptCodeIndexDirContext(ctx, root)
	if err != nil {
		return nil, err
	}

	prefix := strings.ToLower(promptCompletionPrefix(input))
	limit := min(len(index.Symbols), maxCandidates)

	out := make([]promptcomplete.Candidate, 0, limit)
	for _, symbol := range index.Symbols {
		if len(out) >= limit {
			break
		}

		if prefix != "" && !symbolNameMatchesPrefix(symbol.Name, prefix) {
			continue
		}

		rel := relPath(root, symbol.File)
		out = append(out, promptcomplete.Candidate{
			Text:        symbol.Name,
			Kind:        "project-symbol",
			Source:      "project symbol index",
			Description: fmt.Sprintf("%s in %s:%d", symbol.Kind, filepath.ToSlash(rel), symbol.Line),
			Tokens:      []string{symbol.Kind, rel, filepath.Base(rel)},
		})
	}

	return out, nil
}

func countPromptContextGoFiles(ctx context.Context, root string, limit int) (count int, tooMany bool, err error) {
	if ctx == nil {
		return 0, false, promptContextSkipError{reason: "context is empty"}
	}

	if limit <= 0 {
		limit = defaultPromptContextMaxIndexFiles
	}

	err = filepath.WalkDir(root, func(_ string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}

		if ctxErr := ctx.Err(); ctxErr != nil {
			return fmt.Errorf("walk prompt context files: %w", ctxErr)
		}

		name := entry.Name()
		if entry.IsDir() {
			switch name {
			case ".git", ".hg", ".svn", "node_modules", "vendor":
				return filepath.SkipDir
			}

			return nil
		}

		if strings.HasSuffix(name, ".go") {
			count++
			if count > limit {
				tooMany = true

				return fs.SkipAll
			}
		}

		return nil
	})
	if err != nil {
		return count, tooMany, fmt.Errorf("count prompt context Go files: %w", err)
	}

	return count, tooMany, nil
}

func promptGitRecentFileCandidatesWithError(ctx context.Context, root string, maxCandidates int) ([]promptcomplete.Candidate, error) {
	maxCandidates = promptContextCandidateLimit(maxCandidates)

	root = strings.TrimSpace(root)
	if root == "" || ctx == nil {
		return nil, promptContextSkipError{reason: "repository root or context is empty"}
	}

	output, err := runPromptGitOutput(ctx, root, "status", "--short")
	outputTruncated := false

	if err != nil {
		var partialErr promptContextPartialError
		if !errors.As(err, &partialErr) {
			return nil, err
		}

		outputTruncated = partialErr.outputTruncated
	}

	seen := make(map[string]struct{})
	files := make([]string, 0)

	for _, line := range promptGitOutputLines(output, outputTruncated) {
		file := parseGitStatusPath(line)
		if file == "" {
			continue
		}

		if _, ok := seen[file]; ok {
			continue
		}

		seen[file] = struct{}{}

		files = append(files, file)
		if len(files) >= maxCandidates {
			break
		}
	}

	sort.Strings(files)

	out := make([]promptcomplete.Candidate, 0, min(len(files), maxCandidates))
	for _, file := range files {
		if len(out) >= maxCandidates {
			break
		}

		out = append(out, promptcomplete.Candidate{
			Text:        filepath.ToSlash(file),
			Kind:        "recent-file",
			Source:      "git status",
			Description: "recently touched file",
			Tokens:      []string{"git", "status", filepath.Base(file)},
		})
	}

	if err != nil {
		return out, err
	}

	return out, nil
}

func promptGitIssueCandidatesWithError(ctx context.Context, root string, maxCandidates int) ([]promptcomplete.Candidate, error) {
	maxCandidates = promptContextCandidateLimit(maxCandidates)

	root = strings.TrimSpace(root)
	if root == "" || ctx == nil {
		return nil, promptContextSkipError{reason: "repository root or context is empty"}
	}

	output, err := runPromptGitOutput(ctx, root, "branch", "--show-current")
	outputTruncated := false

	if err != nil {
		var partialErr promptContextPartialError
		if !errors.As(err, &partialErr) {
			return nil, err
		}

		outputTruncated = partialErr.outputTruncated
	}

	if outputTruncated {
		output = strings.Join(promptGitOutputLines(output, true), "\n")
	}

	candidates := limitPromptCandidates(issueCandidatesFromTextLimit("git branch", maxCandidates, output), maxCandidates)
	if err != nil {
		return candidates, err
	}

	return candidates, nil
}

func promptGitOutputLines(output string, partial bool) []string {
	lines := strings.Split(output, "\n")
	if partial && !strings.HasSuffix(output, "\n") && len(lines) > 0 {
		return lines[:len(lines)-1]
	}

	return lines
}

func loadPromptTaskCandidatesWithError(
	ctx context.Context,
	store *session.Store,
	root string,
	maxCandidates int,
) ([]promptcomplete.Candidate, error) {
	maxCandidates = promptContextCandidateLimit(maxCandidates)

	if ctx == nil {
		return nil, promptContextSkipError{reason: "context is empty"}
	}

	taskList := taskListPath(store, "")
	taskStore := tasklist.NewStore(taskList)

	tasks, err := taskStore.List(ctx)
	if err != nil {
		return nil, fmt.Errorf("list prompt tasks: %w", err)
	}

	out := make([]promptcomplete.Candidate, 0, min(len(tasks), maxCandidates))
	for i := range tasks {
		if len(out) >= maxCandidates {
			break
		}

		task := &tasks[i]

		description := string(task.Status)
		if task.Title != "" {
			description += ": " + task.Title
		}

		out = append(out, promptcomplete.Candidate{
			Text:        task.ID,
			Kind:        "task",
			Source:      "task list",
			Description: description,
			Tokens:      []string{string(task.Status), task.Agent, root},
		})
	}

	return out, nil
}

func promptTaskCandidates(ctx context.Context, store *session.Store, root string) []promptcomplete.Candidate {
	taskList := taskListPath(store, "")
	if _, err := os.Stat(taskList); err != nil {
		return nil
	}

	candidates, err := loadPromptTaskCandidatesWithError(ctx, store, root, maxPromptContextCandidates)
	if err != nil {
		return nil
	}

	return candidates
}

func filterPromptProjectSymbols(candidates []promptcomplete.Candidate, input string, maxCandidates int) []promptcomplete.Candidate {
	maxCandidates = promptContextCandidateLimit(maxCandidates)

	prefix := strings.ToLower(promptCompletionPrefix(input))
	out := make([]promptcomplete.Candidate, 0, min(len(candidates), maxCandidates))

	for _, candidate := range candidates {
		if len(out) >= maxCandidates {
			break
		}

		if prefix != "" && !symbolNameMatchesPrefix(candidate.Text, prefix) {
			continue
		}

		out = append(out, candidate)
	}

	return out
}

func promptContextCandidateLimit(limits ...int) int {
	if len(limits) == 0 || limits[0] <= 0 {
		return maxPromptContextCandidates
	}

	return limits[0]
}

func limitPromptCandidates(candidates []promptcomplete.Candidate, limit int) []promptcomplete.Candidate {
	if limit <= 0 || len(candidates) <= limit {
		return candidates
	}

	return append([]promptcomplete.Candidate(nil), candidates[:limit]...)
}

func clonePromptRepoContextSnapshot(snapshot promptRepoContextSnapshot) promptRepoContextSnapshot {
	snapshot.ProjectSymbols = clonePromptCandidates(snapshot.ProjectSymbols)
	snapshot.RecentFiles = clonePromptCandidates(snapshot.RecentFiles)
	snapshot.Issues = clonePromptCandidates(snapshot.Issues)
	snapshot.Sources = append([]promptContextSourceStatus(nil), snapshot.Sources...)

	return snapshot
}

func clonePromptCandidates(candidates []promptcomplete.Candidate) []promptcomplete.Candidate {
	if len(candidates) == 0 {
		return nil
	}

	out := make([]promptcomplete.Candidate, len(candidates))
	for i := range candidates {
		out[i] = candidates[i]
		out[i].Tokens = append([]string(nil), candidates[i].Tokens...)
	}

	return out
}

func promptContextSourceReport(source string, status promptContextFreshness, detail string) promptContextSourceStatus {
	return promptContextSourceStatus{
		Source: source,
		Status: status,
		Detail: strings.Join(strings.Fields(detail), " "),
	}
}

func candidateCountDetail(count int) string {
	if count == 1 {
		return "1 candidate"
	}

	return fmt.Sprintf("%d candidates", count)
}

func promptSourcePayloadCandidateCount(payload promptSourcePayload) int {
	return len(payload.ProjectSymbols) + len(payload.RecentFiles) + len(payload.Issues) + len(payload.Tasks)
}

func cleanPromptContextRoot(root string) string {
	root = strings.TrimSpace(root)
	if root == "" {
		return ""
	}

	if abs, err := filepath.Abs(root); err == nil {
		return filepath.Clean(abs)
	}

	return filepath.Clean(root)
}

func writeHashString(h hash.Hash, value string) {
	_, _ = h.Write([]byte(value))
	_, _ = h.Write([]byte{0})
}

func writeHashSampledString(h hash.Hash, value string) {
	writeHashString(h, fmt.Sprintf("len:%d", len(value)))
	writeHashString(h, promptContextStringSample(value))
}

func promptContextStringSample(value string) string {
	if len(value) <= promptSessionRevisionSampleBytes*2 {
		return value
	}

	return value[:promptSessionRevisionSampleBytes] + value[len(value)-promptSessionRevisionSampleBytes:]
}

func formatPromptContextSources(statuses []promptContextSourceStatus) string {
	if len(statuses) == 0 {
		return ""
	}

	var b strings.Builder
	b.WriteString("context:\n")

	for _, status := range statuses {
		if status.Source == "" || status.Status == "" {
			continue
		}

		fmt.Fprintf(&b, "  - %s: %s", status.Source, status.Status)

		if status.Detail != "" {
			fmt.Fprintf(&b, " (%s)", status.Detail)
		}

		b.WriteByte('\n')
	}

	return b.String()
}

func promptContextStatusLabel(statuses []promptContextSourceStatus) string {
	repoStatuses := promptRepoContextStatuses(statuses)

	if label := promptFirstContextStatusLabel(repoStatuses,
		promptContextFreshnessError,
		promptContextFreshnessPartial,
		promptContextFreshnessStale,
	); label != "" {
		return label
	}

	if label := promptFirstContextStatusLabel(statuses,
		promptContextFreshnessError,
		promptContextFreshnessPartial,
		promptContextFreshnessStale,
	); label != "" {
		return label
	}

	if projectSymbols, ok := promptContextStatusForSource(repoStatuses, promptContextSourceProjectSymbols); ok {
		switch projectSymbols.Status {
		case promptContextFreshnessSkipped:
			return string(promptContextFreshnessSkipped) + ":" + promptContextSourceToken(projectSymbols.Source)
		case promptContextFreshnessFresh:
			// In a non-git directory the git-backed sources are expected to be skipped.
			// If the project-symbol snapshot is fresh, summarize the overall prompt
			// context as fresh instead of warning on unavailable git metadata.
			return string(promptContextFreshnessFresh)
		}
	}

	if label := promptFirstContextStatusLabel(repoStatuses, promptContextFreshnessSkipped); label != "" {
		return label
	}

	if len(repoStatuses) > 0 || len(statuses) > 0 {
		return string(promptContextFreshnessFresh)
	}

	return ""
}

func promptRepoContextStatuses(statuses []promptContextSourceStatus) []promptContextSourceStatus {
	repoStatuses := make([]promptContextSourceStatus, 0, 3)

	for _, status := range statuses {
		if isPromptRepoContextSource(status.Source) {
			repoStatuses = append(repoStatuses, status)
		}
	}

	return repoStatuses
}

func promptFirstContextStatusLabel(statuses []promptContextSourceStatus, freshnesses ...promptContextFreshness) string {
	for _, freshness := range freshnesses {
		for _, status := range statuses {
			if status.Status == freshness {
				return string(freshness) + ":" + promptContextSourceToken(status.Source)
			}
		}
	}

	return ""
}

func promptContextStatusForSource(
	statuses []promptContextSourceStatus,
	source string,
) (promptContextSourceStatus, bool) {
	for _, status := range statuses {
		if status.Source == source {
			return status, true
		}
	}

	return promptContextSourceStatus{}, false
}

func isPromptRepoContextSource(source string) bool {
	switch source {
	case promptContextSourceGitBranch, promptContextSourceGitStatus, promptContextSourceProjectSymbols:
		return true
	default:
		return false
	}
}

func promptContextSourceToken(source string) string {
	source = strings.TrimSpace(strings.ToLower(source))

	source = strings.ReplaceAll(source, " ", "-")
	if source == "" {
		return "context"
	}

	return source
}

func promptContextSourceStatusesForSummary(statuses []promptContextSourceStatus) []string {
	out := make([]string, 0, len(statuses))
	for _, status := range statuses {
		if status.Source == "" || status.Status == "" {
			continue
		}

		line := "context " + status.Source + ": " + string(status.Status)
		if status.Detail != "" {
			line += " — " + status.Detail
		}

		out = append(out, line)
	}

	return out
}
