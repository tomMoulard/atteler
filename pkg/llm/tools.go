package llm

// BashTool returns the standard tool definition that lets an LLM execute
// shell commands. The schema follows the OpenAI/Anthropic function-calling
// conventions with a single required "command" parameter.
func BashTool() ToolDefinition {
	return ToolDefinition{
		Name:        "bash",
		Description: "Execute a bash command and return stdout, stderr, and the exit status. Use this to run shell commands, inspect files, run tests, etc.",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"command": map[string]any{
					"type":        "string",
					"description": "The bash command to execute.",
				},
			},
			"required":             []string{"command"},
			"additionalProperties": false,
		},
	}
}

// DefaultTools returns the standard set of tools available to agents.
func DefaultTools() []ToolDefinition {
	return []ToolDefinition{BashTool()}
}
