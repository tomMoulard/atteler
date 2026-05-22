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

agent_loop:
  # Set 0 for no ceiling.
  max_output_bytes: 0
  max_total_tokens: 0

providers:
  openai:
    # base_url: https://api.openai.com
  anthropic:
    # base_url: https://api.anthropic.com
    # Set disable_private_adapter: true to keep direct Anthropic API keys while
    # blocking Anthropic fallback to Claude Code/Forge borrowed credentials.
    # disable_private_adapter: true
  ollama:
    # base_url: http://127.0.0.1:11434
    # Auto-start is opt-in because it launches a local long-lived daemon.
    # Set auto_start: true (or ATTELER_OLLAMA_AUTO_START=true) to let Atteler
    # start "ollama serve" for selected local Ollama runs.
    auto_start: false
  codex:
    # Private/borrowed-credential adapter for Codex CLI ChatGPT login.
    # Set disable_private_adapter: true (or ATTELER_DISABLE_CODEX_ADAPTER=1)
    # to keep normal OpenAI provider support while disabling this adapter.
    # disable_private_adapter: true
  claude-code:
    # Private/borrowed-credential adapter for Claude Code OAuth login.
    # Set disable_private_adapter: true (or ATTELER_DISABLE_CLAUDE_CODE_ADAPTER=1)
    # to keep normal Anthropic provider support while disabling this adapter.
    # disable_private_adapter: true

agents:
  reviewer:
    description: Code review specialist
    capabilities:
      - review
      - security
    model: gpt-4.1
    fallback_models:
      - gpt-4.1-mini
    routing_policy:
      # Prefer providers before cost/latency tie-breakers.
      preferred_providers:
        - openai
      # Reject every model from listed providers.
      # banned_providers:
      #   - ollama
      # Reject provider-qualified IDs or provider-local model names.
      # banned_models:
      #   - openai/gpt-expensive
      # required_capabilities:
      #   - tools
      # max_budget: 0.25
      # require_fresh_metadata: true
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
  # Configured references cross a trust boundary before every model request.
  # Local paths are limited to the working directory plus explicit local_roots.
  # Remote URLs are rejected unless both scheme and host are allowed.
  # references:
  #   - ./docs/style-guide.md
  #   - https://docs.example.com/llm-style.md
  reference_policy:
    # allowed_schemes: [https]
    # allowed_hosts:
    #   - docs.example.com
    # local_roots:
    #   - ../shared-style-guides
    # max_redirects: 0
    # content_types: [text/*, application/json]
    # allow_private_networks: false

plugins:
  # paths:
  #   - ./.atteler/plugins/reviewer
  # policy:
  #   permissions:
  #     filesystem:
  #       read: ["."]
  #       write: []
  #     network:
  #       allow: false
  #       hosts: []
  #     shell:
  #       allow: false
  #     env: []
  #     secrets: []
  #     tools: []
  #   output:
  #     stdout_max_bytes: 65536
  #     stderr_max_bytes: 65536
  #   trusted_install_sources: ["local"]

skill_learning:
  # Set enabled: false to opt out of automatic recurring-workflow skill learning.
  enabled: true
  store_dir: ./.atteler/skill-learning
  skill_dir: ./.atteler/skills/generated
  min_occurrences: 2
  max_steps: 6
  max_observations: 300

#vim: setf=conf
`

// TemplateYAML returns a starter YAML configuration without secrets.
func TemplateYAML() string {
	return templateYAML
}
