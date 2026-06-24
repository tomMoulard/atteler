// Package sdk is Atteler's stable Go SDK facade.
//
// The package intentionally wraps the most common library workflows while
// leaving lower-level packages such as llm, memory, plugin, review, session,
// and worktree available for callers that need full control.
package sdk

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/tommoulard/atteler/pkg/llm"
	"github.com/tommoulard/atteler/pkg/memory"
	"github.com/tommoulard/atteler/pkg/permission"
	"github.com/tommoulard/atteler/pkg/plugin"
	"github.com/tommoulard/atteler/pkg/review"
	"github.com/tommoulard/atteler/pkg/session"
	"github.com/tommoulard/atteler/pkg/worktree"
)

// CompatibilityPolicy describes the source-compatibility promise for the
// stable SDK surface.
const CompatibilityPolicy = "Stable SDK packages preserve exported source compatibility, including exported identifiers unless explicitly deprecated, across patch releases and avoid breaking changes across minor releases without a documented deprecation window; experimental packages may change between releases."

// Stability classifies the compatibility tier of an Atteler import path.
type Stability string

const (
	// APIContractSchemaVersion is the stable JSON schema identifier returned by
	// APIContract.
	APIContractSchemaVersion = "atteler.sdk.v1"

	packagePathPrefix = "github.com/tommoulard/atteler/pkg/"
	packageSinceV010  = "v0.1.0"

	// StabilityStable marks packages covered by CompatibilityPolicy.
	StabilityStable Stability = "stable"
	// StabilityExperimental marks packages intended for advanced use that may
	// change as workflows settle.
	StabilityExperimental Stability = "experimental"
)

// PackageContract documents the SDK compatibility tier for one import path.
type PackageContract struct {
	ImportPath         string    `json:"import_path"`
	Stability          Stability `json:"stability"`
	Since              string    `json:"since"`
	Summary            string    `json:"summary"`
	PrimaryIdentifiers []string  `json:"primary_identifiers,omitempty"`
}

// Contract is the machine-readable SDK compatibility contract.
type Contract struct {
	SchemaVersion       string            `json:"schema_version"`
	CompatibilityPolicy string            `json:"compatibility_policy"`
	Packages            []PackageContract `json:"packages"`
}

func stablePackageContract(name, summary string, identifiers ...string) PackageContract {
	return PackageContract{
		ImportPath:         packagePathPrefix + name,
		Stability:          StabilityStable,
		Since:              packageSinceV010,
		Summary:            summary,
		PrimaryIdentifiers: identifiers,
	}
}

func experimentalPackageContract(name, summary string) PackageContract {
	return PackageContract{
		ImportPath: packagePathPrefix + name,
		Stability:  StabilityExperimental,
		Since:      packageSinceV010,
		Summary:    summary,
	}
}

var packageContracts = []PackageContract{
	stablePackageContract("sdk", "stable facade for common SDK workflows",
		"RunOneShotChat", "NewProviderRegistry", "BuildMemoryIndex", "NewReviewRun",
		"RunPlugin", "NewSession", "AttachNewWorktree", "APIContract",
		"CompatibilityPolicy", "Contract", "PackageContract", "Stability",
		"PackageContracts", "PackagesByStability",
	),
	stablePackageContract("llm", "provider interface, registry, completion, streaming, embeddings, and model metadata",
		"Provider", "Registry", "CompleteParams", "Response", "Message",
		"ToolDefinition", "ResponseFormat",
	),
	stablePackageContract("session", "durable sessions, export, search summaries, headless run metadata, and multi-agent receipts",
		"Session", "Store", "New", "NewStore", "ExportOptions", "HeadlessRun", "MultiAgentRun",
	),
	stablePackageContract("memory", "local text indexing, persistence, and lexical search",
		"Store", "Document", "Result", "NewStore", "Add", "AddFile", "Search", "Save", "Load",
	),
	stablePackageContract("review", "review plans, findings, reports, gates, validation, and text formatting",
		"Reviewer", "Plan", "RunPlanOptions", "NewRunPlan", "Report", "Finding",
		"GateCheck", "ValidateReport", "FormatPlan", "FormatReport",
	),
	stablePackageContract("plugin", "plugin manifests, policy contracts, registries, dry runs, lockfiles, and bounded entrypoint execution",
		"Manifest", "PermissionSet", "Policy", "AcceptManifestPolicy", "Registry",
		"RunOptions", "RunEntrypointWithOptions", "RunResult",
	),
	stablePackageContract("worktree", "context-aware git worktree creation, merge transactions, and cleanup policies",
		"Info", "CreateContext", "MergeWithResultContext", "MergeOptions", "RemoveContext", "WithAuditContext",
	),
	stablePackageContract("retrieval", "shared retrieval query/result contracts used by memory and vector workflows",
		"Query", "Result",
	),
	stablePackageContract("permission", "central side-effect policy and audit decision contracts",
		"Policy", "Request", "Operation", "Decision",
	),
	stablePackageContract("events", "lifecycle event payloads and hook runner contracts",
		"Event", "Runner",
	),
	experimentalPackageContract("agent", "config-backed agent persona registry"),
	experimentalPackageContract("agentmemory", "per-agent vector memory storage"),
	experimentalPackageContract("artifactmerge", "session artifact aggregation and provenance helpers"),
	experimentalPackageContract("async", "dependency-aware task planning primitives"),
	experimentalPackageContract("autonomy", "risk-based capability levels for agent actions"),
	experimentalPackageContract("autopilot", "orchestrator prompt rendering"),
	experimentalPackageContract("codegraph", "dependency-free directed graph primitives for code relationships"),
	experimentalPackageContract("codeintel", "incremental code-intelligence indexes and queries"),
	experimentalPackageContract("config", "layered Atteler configuration loading and migration"),
	experimentalPackageContract("contextpack", "chat-context packing and token-budget helpers"),
	experimentalPackageContract("contextref", "local and URL context-reference expansion"),
	experimentalPackageContract("eval", "text and structured-output evaluation helpers"),
	experimentalPackageContract("feedback", "agent-feedback proposal application helpers"),
	experimentalPackageContract("githistory", "git-log parsing and lexical commit search"),
	experimentalPackageContract("incident", "incident-analysis data structures and renderers"),
	experimentalPackageContract("issuewatch", "local-first issue polling and implementation-run artifact assembly"),
	experimentalPackageContract("lsp", "managed language-server lookups"),
	experimentalPackageContract("mcp", "minimal Model Context Protocol request/response contracts"),
	experimentalPackageContract("modelroute", "model routing catalogs, scoring, and telemetry"),
	experimentalPackageContract("privacy", "conservative redaction helpers"),
	experimentalPackageContract("promptcomplete", "deterministic prompt-line completion"),
	experimentalPackageContract("research", "local-first research artifact creation"),
	experimentalPackageContract("shell", "policy-gated process execution and audit records"),
	experimentalPackageContract("skill", "skill filesystem helpers"),
	experimentalPackageContract("sourcepolicy", "harness source-policy discovery"),
	experimentalPackageContract("speculate", "multi-branch speculation planning"),
	experimentalPackageContract("subagent", "bounded concurrent child-agent spawning primitives"),
	experimentalPackageContract("symphony", "Symphony scheduler client and workflow helpers"),
	experimentalPackageContract("tasklist", "JSON-backed task list for agents"),
	experimentalPackageContract("vector", "local vector indexing and search"),
	experimentalPackageContract("watch", "repository quality scan heuristics and gates"),
}

// APIContract returns the current machine-readable SDK compatibility contract.
func APIContract() Contract {
	return Contract{
		SchemaVersion:       APIContractSchemaVersion,
		CompatibilityPolicy: CompatibilityPolicy,
		Packages:            PackageContracts(),
	}
}

// PackageContracts returns a copy of Atteler's SDK compatibility table.
func PackageContracts() []PackageContract {
	out := make([]PackageContract, len(packageContracts))
	for i := range packageContracts {
		out[i] = packageContracts[i]
		out[i].PrimaryIdentifiers = append([]string(nil), packageContracts[i].PrimaryIdentifiers...)
	}

	return out
}

// PackagesByStability returns SDK contracts matching stability.
func PackagesByStability(stability Stability) []PackageContract {
	contracts := PackageContracts()

	out := make([]PackageContract, 0, len(contracts))
	for _, contract := range contracts {
		if contract.Stability == stability {
			out = append(out, contract)
		}
	}

	return out
}

// NewProviderRegistry creates an LLM registry from caller-supplied providers.
func NewProviderRegistry(providers ...llm.Provider) (*llm.Registry, error) {
	registry := llm.NewRegistry()

	for i, provider := range providers {
		if provider == nil {
			return nil, fmt.Errorf("sdk: provider %d is nil", i)
		}

		if strings.TrimSpace(provider.Name()) == "" {
			return nil, fmt.Errorf("sdk: provider %d has empty name", i)
		}

		registry.Register(provider)
	}

	return registry, nil
}

// OneShotChatOptions configures RunOneShotChat.
type OneShotChatOptions struct {
	Registry       *llm.Registry
	Store          *session.Store
	Temperature    *float64
	TopP           *float64
	Seed           *int
	Session        session.Session
	Model          string
	SystemPrompt   string
	Prompt         string
	FallbackModels []string
	Stop           []string
	MaxTokens      int
	SaveSession    bool
}

// OneShotChatResult is the completion and optional persisted session returned
// by RunOneShotChat.
type OneShotChatResult struct {
	Response    *llm.Response
	Session     session.Session
	SessionPath string
}

// RunOneShotChat sends one user prompt through the provider registry and
// returns the assistant response with an updated session transcript.
func RunOneShotChat(ctx context.Context, options OneShotChatOptions) (OneShotChatResult, error) {
	if ctx == nil {
		return OneShotChatResult{}, llm.ErrContextRequired
	}

	if err := ctx.Err(); err != nil {
		return OneShotChatResult{}, fmt.Errorf("sdk: chat context: %w", err)
	}

	if options.Registry == nil {
		return OneShotChatResult{}, errors.New("sdk: chat registry is required")
	}

	prompt := strings.TrimSpace(options.Prompt)
	if prompt == "" {
		return OneShotChatResult{}, errors.New("sdk: chat prompt is required")
	}

	if options.SaveSession && options.Store == nil {
		return OneShotChatResult{}, errors.New("sdk: chat session store is required when SaveSession is true")
	}

	model := strings.TrimSpace(options.Model)
	if model == "" {
		model = strings.TrimSpace(options.Session.DefaultModel)
	}

	sessionState := prepareChatSession(options.Session, model)

	messages := make([]llm.Message, 0, len(sessionState.Messages)+2)
	if systemPrompt := strings.TrimSpace(options.SystemPrompt); systemPrompt != "" {
		messages = append(messages, llm.Message{Role: llm.RoleSystem, Content: systemPrompt})
	}

	messages = append(messages, sessionState.Messages...)
	messages = append(messages, llm.Message{Role: llm.RoleUser, Content: prompt})

	params := llm.CompleteParams{
		Temperature: options.Temperature,
		TopP:        options.TopP,
		Seed:        options.Seed,
		Model:       model,
		Messages:    messages,
		Stop:        append([]string(nil), options.Stop...),
		MaxTokens:   options.MaxTokens,
	}

	response, err := complete(ctx, options.Registry, params, options.FallbackModels)
	if err != nil {
		return OneShotChatResult{}, fmt.Errorf("sdk: chat complete: %w", err)
	}

	sessionState.Append(llm.RoleUser, prompt)
	sessionState.Append(llm.RoleAssistant, response.Content)

	if response.Model != "" {
		sessionState.DefaultModel = response.Model
	}

	result := OneShotChatResult{
		Response: response,
		Session:  sessionState,
	}

	if options.Store != nil {
		result.SessionPath = options.Store.Path(sessionState.ID)
	}

	if options.SaveSession {
		if err := options.Store.Save(sessionState); err != nil {
			return OneShotChatResult{}, fmt.Errorf("sdk: save chat session: %w", err)
		}
	}

	return result, nil
}

func prepareChatSession(sessionState session.Session, model string) session.Session {
	sessionState.Messages = append([]llm.Message(nil), sessionState.Messages...)

	if sessionState.ID == "" {
		generated := session.New(model, nil)
		sessionState.ID = generated.ID

		if sessionState.CreatedAt.IsZero() {
			sessionState.CreatedAt = generated.CreatedAt
		}

		if sessionState.UpdatedAt.IsZero() {
			sessionState.UpdatedAt = generated.UpdatedAt
		}
	}

	if sessionState.DefaultModel == "" {
		sessionState.DefaultModel = model
	}

	return sessionState
}

// MemoryIndexOptions configures BuildMemoryIndex.
type MemoryIndexOptions struct {
	Documents []memory.Document
	Files     []string
}

// BuildMemoryIndex creates an in-memory lexical index from documents and files.
func BuildMemoryIndex(options MemoryIndexOptions) (*memory.Store, error) {
	store := memory.NewStore()

	for i := range options.Documents {
		document := options.Documents[i]
		if err := store.Add(document); err != nil {
			return nil, fmt.Errorf("sdk: index memory document %q: %w", document.ID, err)
		}
	}

	if len(options.Files) > 0 {
		if err := store.AddFiles(options.Files...); err != nil {
			return nil, fmt.Errorf("sdk: index memory files: %w", err)
		}
	}

	return store, nil
}

// SearchMemory runs a lexical memory search.
func SearchMemory(store *memory.Store, query string, limit int) ([]memory.Result, error) {
	if store == nil {
		return nil, errors.New("sdk: memory store is required")
	}

	results, err := store.Search(query, limit)
	if err != nil {
		return nil, fmt.Errorf("sdk: search memory: %w", err)
	}

	return results, nil
}

// ReviewRunOptions configures NewReviewRun.
type ReviewRunOptions struct {
	Reviewers     []review.Reviewer
	Paths         []string
	RequiredGates []string
}

// ReviewRun is a reusable review workflow contract.
type ReviewRun struct {
	Plan review.Plan
}

// NewReviewRun builds a deterministic review plan for SDK callers.
func NewReviewRun(options ReviewRunOptions) (ReviewRun, error) {
	plan, err := review.NewRunPlan(review.RunPlanOptions{
		Reviewers:     options.Reviewers,
		Paths:         options.Paths,
		RequiredGates: options.RequiredGates,
	})
	if err != nil {
		return ReviewRun{}, fmt.Errorf("sdk: review run plan: %w", err)
	}

	return ReviewRun{Plan: plan}, nil
}

// ValidateReport validates report against the run's required gates.
func (run ReviewRun) ValidateReport(report review.Report) error {
	if err := review.ValidateReport(report, run.Plan.RequiredGates()); err != nil {
		return fmt.Errorf("sdk: review report: %w", err)
	}

	return nil
}

// PluginRunOptions configures RunPlugin.
//
//nolint:govet // Field order follows plugin.RunOptions with root/entrypoint first.
type PluginRunOptions struct {
	Policy         *plugin.Policy
	Permission     *permission.Policy
	Env            map[string]string
	Manifest       plugin.Manifest
	Root           string
	Entrypoint     string
	Autonomy       string
	AuditDir       string
	Args           []string
	Timeout        time.Duration
	AttelerVersion string
}

// RunPlugin executes one plugin entrypoint through the bounded plugin runtime.
func RunPlugin(ctx context.Context, options PluginRunOptions) (plugin.RunResult, error) {
	if ctx == nil {
		return plugin.RunResult{}, llm.ErrContextRequired
	}

	if options.Policy == nil {
		return plugin.RunResult{}, errors.New("sdk: plugin policy is required")
	}

	timeout := options.Timeout
	if timeout <= 0 {
		timeout = 30 * time.Second
	}

	result, err := plugin.RunEntrypointWithOptions(
		ctx,
		options.Root,
		options.Manifest,
		options.Entrypoint,
		plugin.RunOptions{
			Policy:         options.Policy,
			Permission:     options.Permission,
			Env:            options.Env,
			Autonomy:       options.Autonomy,
			AuditDir:       options.AuditDir,
			Timeout:        timeout,
			Args:           options.Args,
			AttelerVersion: options.AttelerVersion,
		},
	)
	if err != nil {
		return result, fmt.Errorf("sdk: run plugin: %w", err)
	}

	return result, nil
}

// SessionOptions configures NewSession.
//
//nolint:govet // Field order follows persisted session schema.
type SessionOptions struct {
	Worktree *worktree.Info
	Messages []llm.Message
	Model    string
	Title    string
	Agent    string
	Tags     []string
	Autonomy string
}

// NewSession creates an unsaved Atteler session with optional worktree
// metadata attached.
func NewSession(options SessionOptions) session.Session {
	sessionState := session.New(options.Model, options.Messages)
	sessionState.Title = strings.TrimSpace(options.Title)
	sessionState.DefaultAgent = strings.TrimSpace(options.Agent)
	sessionState.Autonomy = strings.TrimSpace(options.Autonomy)
	sessionState.Tags = append([]string(nil), options.Tags...)

	if options.Worktree != nil {
		AttachWorktree(&sessionState, options.Worktree)
	}

	return sessionState
}

// SaveSession persists sessionState in store.
func SaveSession(store *session.Store, sessionState session.Session) error {
	if store == nil {
		return errors.New("sdk: session store is required")
	}

	if err := store.Save(sessionState); err != nil {
		return fmt.Errorf("sdk: save session: %w", err)
	}

	return nil
}

// AttachWorktree copies worktree metadata onto sessionState.
func AttachWorktree(sessionState *session.Session, info *worktree.Info) {
	if sessionState == nil || info == nil {
		return
	}

	sessionState.WorktreePath = info.Path
	sessionState.WorktreeBranch = info.Branch
	sessionState.WorktreeBase = info.BaseBranch
}

// AttachNewWorktree creates a git worktree for sessionState and records its
// metadata on the session.
func AttachNewWorktree(ctx context.Context, repoDir string, sessionState *session.Session) (*worktree.Info, error) {
	if sessionState == nil {
		return nil, errors.New("sdk: session is required")
	}

	if strings.TrimSpace(sessionState.ID) == "" {
		return nil, errors.New("sdk: session id is required")
	}

	info, err := worktree.CreateContext(ctx, repoDir, sessionState.ID)
	if err != nil {
		return nil, fmt.Errorf("sdk: create worktree: %w", err)
	}

	AttachWorktree(sessionState, info)

	return info, nil
}

func complete(
	ctx context.Context,
	registry *llm.Registry,
	params llm.CompleteParams,
	fallbackModels []string,
) (*llm.Response, error) {
	if len(fallbackModels) == 0 {
		response, err := registry.Complete(ctx, params)
		if err != nil {
			return nil, fmt.Errorf("registry complete: %w", err)
		}

		return response, nil
	}

	response, err := registry.CompleteWithFallback(ctx, params, fallbackModels)
	if err != nil {
		return nil, fmt.Errorf("registry complete with fallback: %w", err)
	}

	return response, nil
}
