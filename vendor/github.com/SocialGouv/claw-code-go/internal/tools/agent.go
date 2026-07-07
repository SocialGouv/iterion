package tools

import (
	"encoding/json"
	"fmt"
	"github.com/SocialGouv/claw-code-go/internal/api"
	"regexp"
	"strings"
	"time"
)

// defaultAgentModel is empty: a spawn with no explicit model inherits the
// session model (or the subagent type's own override) at spawn time.
const defaultAgentModel = ""

func AgentTool() api.Tool {
	return api.Tool{
		Name: "agent",
		Description: "Spawn a sub-agent with its own conversation loop to handle a delegated task in the background. " +
			"Use it for open-ended exploration or self-contained subtasks so intermediate file dumps land in the sub-agent's context instead of yours. " +
			"Sub-agents are stateless: the prompt must carry ALL needed context (paths, goal, expected report shape). " +
			"The spawn returns a task_id; keep working — you are notified at a later turn when it finishes, and task_output returns its report. " +
			"The report is not shown to the user — relay what matters. " +
			"subagent_type: \"explore\" (read-only search), \"plan\", \"verification\", \"general-purpose\" (default, all tools), or any type you registered with define_subagent.",
		InputSchema: api.InputSchema{
			Type: "object",
			Properties: map[string]api.Property{
				"description":   {Type: "string", Description: "A short (3-5 word) description of the task."},
				"prompt":        {Type: "string", Description: "The task for the agent to perform."},
				"subagent_type": {Type: "string", Description: "Optional agent type specialization."},
				"name":          {Type: "string", Description: "Optional name for the agent."},
				"model":         {Type: "string", Description: "Optional model override."},
			},
			Required: []string{"description", "prompt"},
		},
	}
}

// AgentSpec holds validated agent parameters for spawning.
type AgentSpec struct {
	AgentID      string `json:"agent_id"`
	Name         string `json:"name"`
	Description  string `json:"description"`
	Prompt       string `json:"prompt"`
	SubagentType string `json:"subagent_type"`
	Model        string `json:"model"`
}

// ValidateAgentInput validates agent tool input and returns an AgentSpec.
func ValidateAgentInput(input map[string]any) (*AgentSpec, error) {
	desc, ok := input["description"].(string)
	if !ok || desc == "" {
		return nil, fmt.Errorf("agent: 'description' is required and must not be empty")
	}
	prompt, ok := input["prompt"].(string)
	if !ok || prompt == "" {
		return nil, fmt.Errorf("agent: 'prompt' is required and must not be empty")
	}

	model := defaultAgentModel
	if m, ok := input["model"].(string); ok && m != "" {
		model = m
	}

	subagentType := "general-purpose"
	if st, ok := input["subagent_type"].(string); ok && st != "" {
		subagentType = st
	}

	name := ""
	if n, ok := input["name"].(string); ok && n != "" {
		name = n
	} else {
		name = slugify(desc)
	}

	agentID := fmt.Sprintf("agent-%d", time.Now().UnixNano())

	return &AgentSpec{
		AgentID:      agentID,
		Name:         name,
		Description:  desc,
		Prompt:       prompt,
		SubagentType: subagentType,
		Model:        model,
	}, nil
}

// ExecuteAgent validates input and returns agent metadata.
// Actual goroutine spawning is handled by the ConversationLoop.
func ExecuteAgent(input map[string]any) (string, error) {
	spec, err := ValidateAgentInput(input)
	if err != nil {
		return "", err
	}
	result := map[string]any{
		"agent_id":      spec.AgentID,
		"name":          spec.Name,
		"description":   spec.Description,
		"subagent_type": spec.SubagentType,
		"model":         spec.Model,
		"status":        "running",
		"created_at":    time.Now().UTC().Format(time.RFC3339),
	}
	out, _ := json.MarshalIndent(result, "", "  ")
	return string(out), nil
}

// AllowedToolsForSubagent returns the set of allowed tool names for a subagent type.
func AllowedToolsForSubagent(subagentType string) map[string]bool {
	explorTools := map[string]bool{
		"read_file":         true,
		"glob":              true,
		"grep":              true,
		"semantic_search":   true,
		"web_fetch":         true,
		"web_search":        true,
		"tool_search":       true,
		"skill":             true,
		"structured_output": true,
	}

	switch strings.ToLower(subagentType) {
	case "explore":
		return explorTools
	case "plan":
		plan := make(map[string]bool)
		for k, v := range explorTools {
			plan[k] = v
		}
		plan["todo_write"] = true
		plan["send_user_message"] = true
		return plan
	case "verification":
		plan := AllowedToolsForSubagent("plan")
		plan["bash"] = true
		return plan
	default:
		// general-purpose: all tools allowed
		return nil
	}
}

var nonAlphanumRe = regexp.MustCompile(`[^a-z0-9]+`)

func slugify(s string) string {
	s = strings.ToLower(s)
	s = nonAlphanumRe.ReplaceAllString(s, "-")
	s = strings.Trim(s, "-")
	if len(s) > 40 {
		s = s[:40]
	}
	return s
}

// SlugifyAgentLabel derives a short display label from free text (exported
// for the workflow engine's default agent labels).
func SlugifyAgentLabel(s string) string { return slugify(s) }
