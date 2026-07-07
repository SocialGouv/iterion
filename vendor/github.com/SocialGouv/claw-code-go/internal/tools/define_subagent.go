package tools

import (
	"fmt"
	"strings"

	"github.com/SocialGouv/claw-code-go/internal/api"
)

// SubagentSpec is the validated input of the define_subagent tool. The
// conversation loop turns it into a session-scoped subagent type.
type SubagentSpec struct {
	Name         string   `json:"name"`
	Description  string   `json:"description,omitempty"`
	SystemPrompt string   `json:"system_prompt"`
	AllowedTools []string `json:"allowed_tools,omitempty"`
	Model        string   `json:"model,omitempty"`
}

// DefineSubagentTool returns the tool definition for defining a subagent
// type at runtime.
func DefineSubagentTool() api.Tool {
	return api.Tool{
		Name: "define_subagent",
		Description: "Define a named subagent type for THIS session — a system prompt plus an optional " +
			"tool allow-list — then spawn it with the agent tool (subagent_type = the name). " +
			"Use it when a recurring delegated role needs its own persona or a restricted toolset " +
			"(e.g. a read-only reviewer, a test-runner). Redefining the same name replaces it; " +
			"built-in types (explore, plan, verification, general-purpose) cannot be shadowed.",
		InputSchema: api.InputSchema{
			Type: "object",
			Properties: map[string]api.Property{
				"name":          {Type: "string", Description: "Subagent type name (used as agent subagent_type)."},
				"description":   {Type: "string", Description: "One-line description of the role."},
				"system_prompt": {Type: "string", Description: "The subagent's system prompt (replaces the built-in base; context sections still apply)."},
				"allowed_tools": {Type: "array", Description: "Tool names the subagent may use (empty = all tools except orchestration ones).",
					Items: &api.Property{Type: "string"}},
				"model": {Type: "string", Description: "Optional model override for this subagent type."},
			},
			Required: []string{"name", "system_prompt"},
		},
	}
}

// ValidateDefineSubagentInput validates define_subagent input.
func ValidateDefineSubagentInput(input map[string]any) (*SubagentSpec, error) {
	spec := &SubagentSpec{
		Name:         strings.TrimSpace(stringVal(input, "name")),
		Description:  strings.TrimSpace(stringVal(input, "description")),
		SystemPrompt: strings.TrimSpace(stringVal(input, "system_prompt")),
		AllowedTools: stringSlice(input, "allowed_tools"),
		Model:        strings.TrimSpace(stringVal(input, "model")),
	}
	if spec.Name == "" {
		return nil, fmt.Errorf("define_subagent: 'name' is required")
	}
	if spec.SystemPrompt == "" {
		return nil, fmt.Errorf("define_subagent: 'system_prompt' is required")
	}
	return spec, nil
}
