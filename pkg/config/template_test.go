package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestTemplateYAML(t *testing.T) {
	t.Parallel()

	template := TemplateYAML()
	for _, want := range []string{
		"default_provider:",
		"version:",
		"generation:",
		"agent_loop:",
		"max_output_bytes:",
		"max_total_tokens:",
		"providers:",
		"agents:",
		"routing_policy:",
		"hooks:",
		"context:",
		"plugins:",
		"policy:",
		"trusted_install_sources:",
		"vector:",
		"vectorizer: lexical",
		"fallback_policy: fail",
	} {
		if !strings.Contains(template, want) {
			require.Failf(t, "unexpected failure", "TemplateYAML missing %q in:\n%s", want, template)
		}
	}

	if strings.Contains(template, "api.openai.com/v1") {
		require.Failf(t, "unexpected failure", "TemplateYAML should use OpenAI host root, got:\n%s", template)
	}

	path := filepath.Join(t.TempDir(), "config.yaml")
	require.NoError(t, os.WriteFile(path, []byte(template), 0o600))

	cfg, _, err := LoadFiles([]string{path})
	require.NoError(t, err)
	assert.Equal(t, ConfigSchemaVersion, cfg.Version)
	assert.Equal(t, starterTemplateConfig(), cfg)
}
