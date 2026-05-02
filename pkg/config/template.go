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
  seed: 1
  reasoning_level: medium
  max_tokens: 2048

providers:
  openai:
    # base_url: https://api.openai.com
  anthropic:
    # base_url: https://api.anthropic.com
  ollama:
    # base_url: http://127.0.0.1:11434
    # Atteler auto-starts "ollama serve" for selected local Ollama runs unless
    # ATTELER_OLLAMA_AUTO_START=false is set.

agents:
  reviewer:
    description: Code review specialist
    capabilities:
      - review
      - security
    model: gpt-4.1
    fallback_models:
      - gpt-4.1-mini
    seed: 1
    reasoning_level: high
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
  max_input_tokens: 120000

plugins:
  # paths:
  #   - ./.atteler/plugins/reviewer
`

// TemplateYAML returns a starter YAML configuration without secrets.
func TemplateYAML() string {
	return templateYAML
}
