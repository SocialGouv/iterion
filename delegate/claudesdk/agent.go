package claudesdk

// AgentDefinition defines a subagent configuration.
type AgentDefinition struct {
	// Model is the Claude model to use for this agent.
	Model string `json:"model,omitempty"`

	// SystemPrompt overrides the system prompt for this agent.
	SystemPrompt string `json:"system_prompt,omitempty"`

	// AllowedTools restricts which tools the agent can use.
	AllowedTools []string `json:"allowed_tools,omitempty"`

	// DisallowedTools prevents the agent from using specific tools.
	DisallowedTools []string `json:"disallowed_tools,omitempty"`

	// MaxTurns limits the number of turns for this agent.
	MaxTurns *int `json:"max_turns,omitempty"`
}
