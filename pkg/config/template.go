package config

const templateYAML = `# Atteler configuration
# Save as ~/.config/atteler/config.yaml, ./.atteler/config.yaml, or ./.atteler.yaml.

default_provider: openai
default_model: gpt-4.1
fallback_models:
  - gpt-4.1-mini

generation:
  temperature: 0
  top_p: 1
  max_tokens: 2048

providers:
  openai:
    # base_url: https://api.openai.com
  anthropic:
    # base_url: https://api.anthropic.com

agents:
  reviewer:
    model: gpt-4.1
    fallback_models:
      - gpt-4.1-mini
    temperature: 0
    max_tokens: 2048
    triggers:
      - "review this"
      - "code review"
    system_prompt: |
      You are a concise code reviewer. Focus on correctness, tests,
      security, and maintainability.

hooks:
  session_end:
    - command: ["echo", "atteler session ended"]
      timeout_seconds: 5

context:
  max_file_bytes: 32768
  max_total_bytes: 131072
`

// TemplateYAML returns a starter YAML configuration without secrets.
func TemplateYAML() string {
	return templateYAML
}
