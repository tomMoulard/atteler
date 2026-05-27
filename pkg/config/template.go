package config

import (
	"fmt"
	"strings"

	"gopkg.in/yaml.v3"

	attelerplugin "github.com/tommoulard/atteler/pkg/plugin"
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
	retryMaxAttempts := 2
	retryInitialBackoffMS := 1000
	retryMaxBackoffMS := 10000
	retryMaxElapsedMS := 30000
	retryJitterFraction := 0.2

	return Config{
		Version:         ConfigSchemaVersion,
		DefaultProvider: templateDefaultProvider,
		DefaultModel:    templateDefaultModel,
		FallbackModels:  []string{templateFallbackModel},
		ModelAliases: map[string]string{
			"fast": templateDefaultProvider + "/" + templateFallbackModel,
		},
		Generation: GenerationConfig{
			Temperature:    &temperature,
			TopP:           &topP,
			Seed:           &seed,
			ReasoningLevel: "medium",
			MaxTokens:      2048,
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
	out.WriteString("# Hooks default to inherit_env: false; set true only when the hook needs PATH, HOME, or credentials.\n")
	out.WriteString("# Explicit env values are passed verbatim to the hook process; avoid putting credentials there unless needed.\n")
	out.WriteString("# Event ATTELER_* variables are reserved and generated from sanitized event data.\n")
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
	out.Write(data)
	out.WriteString("\n#vim: setf=conf\n")

	return out.String()
}

// TemplateYAML returns a starter YAML configuration without secrets.
func TemplateYAML() string {
	return templateYAML()
}
