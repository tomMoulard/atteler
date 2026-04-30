package config

import (
	"strings"
	"testing"
)

func TestTemplateYAML(t *testing.T) {
	template := TemplateYAML()
	for _, want := range []string{
		"default_provider:",
		"generation:",
		"providers:",
		"agents:",
		"hooks:",
		"context:",
	} {
		if !strings.Contains(template, want) {
			t.Fatalf("TemplateYAML missing %q in:\n%s", want, template)
		}
	}
	if strings.Contains(template, "api.openai.com/v1") {
		t.Fatalf("TemplateYAML should use OpenAI host root, got:\n%s", template)
	}
}
