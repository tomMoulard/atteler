package config

import (
	"fmt"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/tommoulard/atteler/pkg/autonomy"
	attelerplugin "github.com/tommoulard/atteler/pkg/plugin"
	"github.com/tommoulard/atteler/pkg/sourcepolicy"
)

const (
	templateDefaultProvider = "openai"
	templateDefaultModel    = "gpt-4.1"
	templateFallbackModel   = "gpt-4.1-mini"
)

// starterTemplateConfig returns the example config that backs TemplateYAML.
// Keeping the starter as a Config value makes the emitted YAML follow the same
// schema tags that the loader accepts, so new fields are less likely to drift
// from the on-disk shape.
func starterTemplateConfig() Config {
	temperature := 0.0
	topP := 1.0
	seed := 1
	agentLoopMaxOutputBytes := int64(0)
	agentLoopMaxCostMicros := int64(0)
	agentLoopMaxInputTokens := 0
	agentLoopMaxOutputTokens := 0
	agentLoopMaxTotalTokens := 0
	agentLoopMaxIterations := 0
	agentLoopMaxModelCalls := 0
	agentLoopMaxToolCalls := 0
	agentLoopWallTime := "0"
	agentLoopCheckpointInterval := 0
	skillLearningEnabled := true
	workspaceVectorEnabled := false
	workspaceAllowRemoteEmbeddings := false
	allowLowTrustSources := true
	warnOnLowTrustSources := true
	requireEvidenceForHighImpactClaims := false
	retryMaxAttempts := 2
	retryInitialBackoffMS := 1000
	retryMaxBackoffMS := 10000
	retryMaxElapsedMS := 30000
	retryJitterFraction := 0.2
	worktreeAutoMerge := false

	return Config{
		Version:         ConfigSchemaVersion,
		DefaultProvider: templateDefaultProvider,
		DefaultModel:    templateDefaultModel,
		FallbackModels:  []string{templateFallbackModel},
		Autonomy:        autonomy.DefaultLevel.String(),
		ModelAliases: map[string]string{
			"mini": templateDefaultProvider + "/" + templateFallbackModel,
		},
		ModelRoles: map[string]ModelRoleConfig{
			"planner": {
				Preferred:      templateDefaultProvider + "/" + templateDefaultModel,
				FallbackModels: []string{templateDefaultProvider + "/" + templateFallbackModel},
				RequiredCapabilities: []string{
					"tools",
					"json_schema",
				},
				MaxCostUSD:   0.25,
				MaxLatencyMS: 2500,
				MaxTTFTMS:    900,
			},
			"fast_coder": {
				Preferred:      templateDefaultProvider + "/" + templateFallbackModel,
				FallbackModels: []string{"ollama/llama3.2"},
				PreferLocal:    true,
			},
		},
		Generation: GenerationConfig{
			Temperature:    &temperature,
			TopP:           &topP,
			Seed:           &seed,
			ModelMode:      "default",
			ReasoningLevel: "medium",
			MaxTokens:      2048,
		},
		Research: ResearchConfig{
			SourcePolicy: sourcepolicy.Policy{
				TrustedDomains: []string{
					"github.com",
					"go.dev",
				},
				DeniedDomains: []string{
					"example-content-farm.com",
				},
				PreferSourceTypes: []string{
					sourcepolicy.SourceTypeIssueDiscussion,
					sourcepolicy.SourceTypeOfficialDocs,
					sourcepolicy.SourceTypeSourceCode,
					sourcepolicy.SourceTypeStandardOrSpec,
				},
				AllowLowTrustSources:               &allowLowTrustSources,
				WarnOnLowTrustSources:              &warnOnLowTrustSources,
				RequireEvidenceForHighImpactClaims: &requireEvidenceForHighImpactClaims,
			},
		},
		AgentLoop: AgentLoopConfig{
			MaxOutputBytes:     &agentLoopMaxOutputBytes,
			MaxCostMicros:      &agentLoopMaxCostMicros,
			MaxInputTokens:     &agentLoopMaxInputTokens,
			MaxOutputTokens:    &agentLoopMaxOutputTokens,
			MaxTotalTokens:     &agentLoopMaxTotalTokens,
			MaxIterations:      &agentLoopMaxIterations,
			MaxModelCalls:      &agentLoopMaxModelCalls,
			MaxToolCalls:       &agentLoopMaxToolCalls,
			MaxWallTime:        &agentLoopWallTime,
			CheckpointInterval: &agentLoopCheckpointInterval,
		},
		Providers: map[string]ProviderConfig{
			"claude-code": {},
			"codex":       {},
			"anthropic":   {},
			templateDefaultProvider: {
				Retry: RetryConfig{
					MaxAttempts:      &retryMaxAttempts,
					InitialBackoffMS: &retryInitialBackoffMS,
					MaxBackoffMS:     &retryMaxBackoffMS,
					MaxElapsedMS:     &retryMaxElapsedMS,
					JitterFraction:   &retryJitterFraction,
				},
			},
			"ollama": {BaseURL: "http://127.0.0.1:11434"},
			"vllm": {
				Type:         "openai_compatible",
				BaseURL:      "http://127.0.0.1:8000",
				Models:       []string{"qwen2.5-coder"},
				Capabilities: []string{"chat", "tools", "json_schema", "local"},
				Local:        true,
			},
		},
		Agents: map[string]AgentConfig{
			"reviewer": {
				Description:    "Code review specialist",
				Capabilities:   []string{"review", "security"},
				Model:          templateDefaultModel,
				FallbackModels: []string{templateFallbackModel},
				RoutingPolicy: RoutingPolicyConfig{
					PreferredProviders: []string{templateDefaultProvider},
				},
				Seed:           &seed,
				ReasoningLevel: "high",
				Temperature:    &temperature,
				MaxTokens:      2048,
				Triggers:       []string{"review this", "code review"},
				SystemPrompt: "You are a concise code reviewer. Focus on correctness, " +
					"tests, security, and maintainability.",
			},
		},
		Hooks: map[string][]HookConfig{
			"session_end": {{
				Command:        []string{"echo", "atteler session ended"},
				Payload:        "metadata",
				TimeoutSeconds: 5,
			}},
		},
		Context: ContextConfig{
			MaxFileBytes:   32768,
			MaxTotalBytes:  131072,
			MaxInputTokens: 120000,
			ReferencePolicy: ReferencePolicyConfig{
				AllowedSchemes: []string{"https"},
				AllowedHosts:   []string{"docs.example.com"},
				LocalRoots:     []string{"../shared-style-guides"},
				ContentTypes:   []string{"text/*", "application/json"},
			},
		},
		Plugins: PluginConfig{
			Policy: &attelerplugin.Policy{
				Permissions: attelerplugin.PermissionSet{
					Filesystem: attelerplugin.FilesystemPermissions{
						Read: []string{"."},
					},
					Network: attelerplugin.NetworkPermissions{Allow: false},
					Shell:   attelerplugin.ShellPermissions{Allow: false},
				},
				Output: attelerplugin.OutputLimits{
					StdoutMaxBytes: 65536,
					StderrMaxBytes: 65536,
				},
				TrustedInstallSources: []string{"local"},
			},
		},
		SkillLearning: SkillLearningConfig{
			Enabled:         &skillLearningEnabled,
			StoreDir:        "./.atteler/skill-learning",
			SkillDir:        "./.atteler/skills/generated",
			MaxObservations: 300,
			MaxSteps:        6,
			MinOccurrences:  2,
		},
		Vector: VectorConfig{
			WorkspaceEnabled:               &workspaceVectorEnabled,
			WorkspaceAllowRemoteEmbeddings: &workspaceAllowRemoteEmbeddings,
			Vectorizer:                     "lexical",
			IndexPath:                      "./.atteler/vector-index.json",
			WorkspaceIndexPath:             "./.atteler/workspace-vector-index.json",
			Provider:                       "ollama",
			Model:                          "nomic-embed-text",
			BaseURL:                        "http://127.0.0.1:11434",
			TimeoutSeconds:                 30,
			FallbackPolicy:                 "fail",
			ChunkMaxRunes:                  1200,
			ChunkOverlapRunes:              120,
			WorkspaceLimit:                 4,
			WorkspaceMaxFileBytes:          262144,
			WorkspaceMaxFiles:              5000,
			WorkspaceExclude:               []string{"tmp/", "*.generated.*"},
		},
		Worktree: WorktreeConfig{
			AutoMerge:            &worktreeAutoMerge,
			VerificationCommands: []string{"go test ./..."},
			OverrideVerification: false,
		},
	}
}

func templateYAML() string {
	data, err := yaml.Marshal(starterTemplateConfig())
	if err != nil {
		panic(fmt.Sprintf("marshal starter config template: %v", err))
	}

	var out strings.Builder
	out.WriteString("# Atteler configuration\n")
	out.WriteString("# Save as ~/.config/atteler/config.yaml, ./.atteler/config.yaml, or ./.atteler.yaml.\n")
	out.WriteString("# Generated from the current config schema and starter defaults.\n")
	out.WriteString("# Use `atteler config explain` to inspect implicit defaults and merge provenance.\n")
	out.WriteString("# Lifecycle hook payload defaults to metadata: no prompt text, command output, file paths, or raw errors.\n")
	out.WriteString("# Use payload: summary for bounded redacted summaries, or payload: full only for trusted hooks.\n")
	out.WriteString("# Hooks default to blocking: false; set true only for safety-critical hooks that may fail the caller.\n")
	out.WriteString("# Hooks default to inherit_env: false; set true only when the hook needs PATH, HOME, or credentials.\n")
	out.WriteString("# Explicit env values are passed verbatim to the hook process; avoid putting credentials there unless needed.\n")
	out.WriteString("# Event ATTELER_* variables are reserved and generated from sanitized event data.\n")
	out.WriteString("# Set event_ledger_path to persist a redacted append-only lifecycle JSONL ledger.\n")
	out.WriteString("# Research source policy is evidence-first, not evidence-only: it prefers and labels strong sources,\n")
	out.WriteString("# excludes denied domains, and warns on weak evidence without making citations mandatory by default.\n")
	out.WriteString("# Configured references cross a trust boundary before every model request.\n")
	out.WriteString("# Remote URLs are rejected unless both scheme and host are allowed below.\n\n")
	out.WriteString("# Local paths are limited to the working directory plus explicit local_roots; absolute paths require allow_absolute_paths.\n")
	out.WriteString("# Private-network URL targets remain blocked unless allow_private_networks is set deliberately.\n")
	out.WriteString("\n")
	out.WriteString("# Workspace vector indexing is disabled by default. If vector.vectorizer is\n")
	out.WriteString("# changed to embedding, indexed file chunks are sent to vector.base_url;\n")
	out.WriteString("# non-loopback embedding endpoints also require workspace_allow_remote_embeddings: true.\n\n")
	out.WriteString("# Set vector.fallback_policy: lexical to stay local with lexical workspace search\n")
	out.WriteString("# when an embedding endpoint is unavailable or not explicitly consented.\n\n")
	out.WriteString("# Top-level vector.index_path is the generic file-vector search store path.\n")
	out.WriteString("# Workspace vectors use vector.workspace_index_path or vector.stores.workspace.index_path.\n")
	out.WriteString("# Do not share one index_path across workspace, file, session, git, ADR, or agent-memory indexes.\n")
	out.WriteString("# Use vector.stores.<name>, vector.agents.<name>, and vector.sources.<kind>\n")
	out.WriteString("# for per-agent memory plus session, git-history, ADR, and file-source indexes.\n\n")
	out.WriteString("# Supported vector store scopes: agent-memory, vector-search, workspace.\n")
	out.WriteString("# Supported vector source scopes: file, session, git_history, adr.\n\n")
	out.WriteString("# Vector agent scopes must match configured agent names.\n\n")
	out.WriteString("# Worktree isolation preserves session worktrees by default. Set\n")
	out.WriteString("# worktree.auto_merge: true only with reviewed verification_commands, or set\n")
	out.WriteString("# override_verification: true as an explicit no-verification override when no\n")
	out.WriteString("# verification commands are supplied.\n\n")
	out.Write(data)
	out.WriteString("\n#vim: setf=conf\n")

	return out.String()
}

// TemplateYAML returns a starter YAML configuration without secrets.
func TemplateYAML() string {
	return templateYAML()
}
