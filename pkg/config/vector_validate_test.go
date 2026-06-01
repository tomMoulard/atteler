package config

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestValidateVectorConfigAcceptsSupportedScopesAndAliases(t *testing.T) {
	t.Parallel()

	err := ValidateVectorConfig(VectorConfig{
		Vectorizer:     "lexical_fallback",
		Provider:       "ollama_compatible",
		FallbackPolicy: "none",
		Stores: map[string]VectorizerConfig{
			"agent-memory":  {Vectorizer: "embedding"},
			"vector-search": {Vectorizer: "hashed"},
			"workspace":     {FallbackPolicy: "lexical"},
		},
		Agents: map[string]VectorizerConfig{
			"reviewer": {Vectorizer: "embed", Provider: "ollama"},
		},
		Sources: map[string]VectorizerConfig{
			"file":        {Vectorizer: "text"},
			"session":     {Vectorizer: "embeddings"},
			"git_history": {Vectorizer: "lexical"},
			"adr":         {FallbackPolicy: "fail"},
		},
	})
	require.NoError(t, err)
}

func TestValidateVectorConfigRejectsUnsupportedScopesAndValues(t *testing.T) {
	t.Parallel()

	err := ValidateVectorConfig(VectorConfig{
		Vectorizer:     "semantic",
		Provider:       "openai",
		FallbackPolicy: "retry",
		Stores: map[string]VectorizerConfig{
			"agentmemory": {Vectorizer: "dense"},
		},
		Agents: map[string]VectorizerConfig{
			"reviewer": {Provider: "anthropic"},
		},
		Sources: map[string]VectorizerConfig{
			"git_histry": {FallbackPolicy: "remote"},
		},
	})
	require.Error(t, err)

	message := err.Error()
	assert.Contains(t, message, `vector.vectorizer unsupported value "semantic"`)
	assert.Contains(t, message, `vector.provider unsupported value "openai"`)
	assert.Contains(t, message, `vector.fallback_policy unsupported value "retry"`)
	assert.Contains(t, message, "vector.stores.agentmemory unknown store scope")
	assert.Contains(t, message, `vector.stores.agentmemory.vectorizer unsupported value "dense"`)
	assert.Contains(t, message, `vector.agents.reviewer.provider unsupported value "anthropic"`)
	assert.Contains(t, message, "vector.sources.git_histry unknown source scope")
	assert.Contains(t, message, `vector.sources.git_histry.fallback_policy unsupported value "remote"`)
}

func TestValidateVectorConfigWithAgentsChecksAgentScopes(t *testing.T) {
	t.Parallel()

	err := ValidateVectorConfigWithAgents(VectorConfig{
		Agents: map[string]VectorizerConfig{
			"review-team": {Vectorizer: "embedding"},
		},
	}, map[string]AgentConfig{
		"Review_Team": {},
	})
	require.NoError(t, err)

	err = ValidateVectorConfigWithAgents(VectorConfig{
		Agents: map[string]VectorizerConfig{
			"reviwer": {Vectorizer: "embedding"},
		},
	}, map[string]AgentConfig{
		"reviewer": {},
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "vector.agents.reviwer unknown agent scope")
}

func TestValidateVectorConfigRejectsIndexPathCollisionsAcrossLifecycles(t *testing.T) {
	t.Parallel()

	err := ValidateVectorConfig(VectorConfig{
		WorkspaceIndexPath: "./.atteler/shared-index.json",
		Sources: map[string]VectorizerConfig{
			"file": {
				IndexPath: ".atteler/shared-index.json",
			},
			"session": {
				IndexPath: ".atteler/session-index.json",
			},
			"git_history": {
				IndexPath: "./.atteler/session-index.json",
			},
		},
	})
	require.Error(t, err)

	message := err.Error()
	assert.Contains(t, message, "vector.sources.file index_path")
	assert.Contains(t, message, "file and workspace indexes must not share")
	assert.Contains(t, message, "vector.sources.session index_path")
	assert.Contains(t, message, "session and git-history indexes must not share")
}

func TestValidateVectorConfigAllowsIndexPathAliasesWithinLifecycle(t *testing.T) {
	t.Parallel()

	err := ValidateVectorConfig(VectorConfig{
		IndexPath:          ".atteler/file-index.json",
		WorkspaceIndexPath: ".atteler/workspace-index.json",
		Stores: map[string]VectorizerConfig{
			"vector-search": {IndexPath: "./.atteler/file-index.json"},
			"workspace":     {IndexPath: "./.atteler/workspace-index.json"},
			"agent-memory":  {IndexPath: ".atteler/agent-memory.json"},
		},
		Agents: map[string]VectorizerConfig{
			"reviewer": {IndexPath: "./.atteler/agent-memory.json"},
		},
		Sources: map[string]VectorizerConfig{
			"file": {IndexPath: "./.atteler/file-index.json"},
		},
	})
	require.NoError(t, err)
}

func TestValidateVectorConfigRejectsIndexPathCollisionsWithDefaults(t *testing.T) {
	t.Parallel()

	err := ValidateVectorConfig(VectorConfig{
		WorkspaceIndexPath: ".atteler/agent-memory.json",
		Sources: map[string]VectorizerConfig{
			"session": {
				IndexPath: ".atteler/vector-index.json",
			},
		},
	})
	require.Error(t, err)

	message := err.Error()
	assert.Contains(t, message, "vector.workspace_index_path index_path")
	assert.Contains(t, message, "workspace and agent-memory indexes must not share")
	assert.Contains(t, message, "vector.sources.session index_path")
	assert.Contains(t, message, "session and file indexes must not share")
}

func TestValidateVectorConfigWithAgentsRejectsSharedMemoryPathForDifferentVectorizers(t *testing.T) {
	t.Parallel()

	err := ValidateVectorConfigWithAgents(VectorConfig{
		Stores: map[string]VectorizerConfig{
			"agent-memory": {
				Vectorizer: "embedding",
				Model:      "shared-memory-embed",
				IndexPath:  ".atteler/agent-memory.json",
			},
		},
		Agents: map[string]VectorizerConfig{
			"reviewer": {
				Model: "reviewer-memory-embed",
			},
		},
	}, map[string]AgentConfig{
		"planner":  {},
		"reviewer": {},
	})
	require.Error(t, err)

	message := err.Error()
	assert.Contains(t, message, "vector.agents.reviewer shares agent-memory index_path")
	assert.Contains(t, message, "different vectorizer identity")
}

func TestValidateVectorConfigWithAgentsAllowsDistinctMemoryPathsForDifferentVectorizers(t *testing.T) {
	t.Parallel()

	err := ValidateVectorConfigWithAgents(VectorConfig{
		Stores: map[string]VectorizerConfig{
			"agent-memory": {
				Vectorizer: "embedding",
				Model:      "shared-memory-embed",
				IndexPath:  ".atteler/agent-memory.json",
			},
		},
		Agents: map[string]VectorizerConfig{
			"reviewer": {
				Model:     "reviewer-memory-embed",
				IndexPath: ".atteler/reviewer-memory.json",
			},
		},
	}, map[string]AgentConfig{
		"planner":  {},
		"reviewer": {},
	})
	require.NoError(t, err)
}
