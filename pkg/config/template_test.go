package config

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestTemplateYAML(t *testing.T) {
	t.Parallel()

	template := TemplateYAML()
	for _, want := range []string{
		"default_provider:",
		"generation:",
		"agent_loop:",
		"max_output_bytes:",
		"max_total_tokens:",
		"providers:",
		"agents:",
		"hooks:",
		"context:",
		"plugins:",
		"policy:",
		"trusted_install_sources:",
	} {
		if !strings.Contains(template, want) {
			require.Failf(t, "unexpected failure", "TemplateYAML missing %q in:\n%s", want, template)
		}
	}

	if strings.Contains(template, "api.openai.com/v1") {
		require.Failf(t, "unexpected failure", "TemplateYAML should use OpenAI host root, got:\n%s", template)
	}
}
