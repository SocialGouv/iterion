package delegate

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/SocialGouv/iterion/claw/coding"
	"github.com/zendev-sh/goai"
	"github.com/zendev-sh/goai/provider"

	"github.com/SocialGouv/iterion/tool"
)

// ClawBackend delegates node execution to an in-process claw CodingAgent.
// Unlike ClaudeCodeBackend and CodexBackend which spawn CLI subprocesses,
// ClawBackend runs the agent loop in-process, using any goai LanguageModel.
type ClawBackend struct {
	// ModelFactory creates a LanguageModel for each execution.
	// Called once per Execute call to allow per-node model configuration.
	ModelFactory func() (provider.LanguageModel, error)

	// ToolRegistry is the iterion tool registry. When set, MCP tools
	// registered there are passed as extra tools to the claw agent.
	ToolRegistry *tool.Registry
}

// Execute runs a claw CodingAgent in-process with the given task.
func (b *ClawBackend) Execute(ctx context.Context, task Task) (Result, error) {
	if task.WorkDir != "" {
		if err := validateWorkDir(task.WorkDir, task.BaseDir); err != nil {
			return Result{}, err
		}
	}

	if b.ModelFactory == nil {
		return Result{}, fmt.Errorf("delegate: claw: ModelFactory is nil")
	}

	model, err := b.ModelFactory()
	if err != nil {
		return Result{}, fmt.Errorf("delegate: claw model: %w", err)
	}

	// Collect MCP tools from the registry if available.
	var extraTools []goai.Tool
	if b.ToolRegistry != nil {
		extraTools = registryToolsAsGoai(b.ToolRegistry)
	}

	startTime := time.Now()

	// Build the system prompt for the agent.
	systemPrompt := task.SystemPrompt
	if task.InteractionEnabled {
		systemPrompt += interactionSystemInstruction
	}

	// CompactionMaxTokens is intentionally unset: delegated tasks are typically
	// short-lived, so context compaction is not needed.
	agent, err := coding.New(coding.CodingAgentConfig{
		Model:          model,
		WorkDir:        task.WorkDir,
		SystemPrompt:   systemPrompt,
		PermissionMode: "bypass", // iterion manages security at the orchestration level
		AllowedTools:   task.AllowedTools,
		ExtraTools:     extraTools,
		SessionID:      task.SessionID,
	})
	if err != nil {
		return Result{}, fmt.Errorf("delegate: claw init: %w", err)
	}
	defer agent.Close()

	// Build prompt, embedding output schema if both schema and tools are present.
	prompt := task.UserPrompt
	if len(task.OutputSchema) > 0 && len(task.AllowedTools) > 0 {
		prompt += "\n\nRespond with JSON matching this schema:\n" + string(task.OutputSchema)
	}

	result, err := agent.Run(ctx, prompt)
	duration := time.Since(startTime)

	if err != nil {
		return Result{
			Duration:    duration,
			BackendName: "claw",
		}, fmt.Errorf("delegate: claw: %w", err)
	}

	output, rawLen, fallback := parseTextOutput(result.Text, task.OutputSchema)
	return Result{
		Output:        output,
		Tokens:        result.Usage.InputTokens + result.Usage.OutputTokens,
		Duration:      duration,
		BackendName:   "claw",
		RawOutputLen:  rawLen,
		ParseFallback: fallback,
		SessionID:     agent.SessionID(),
	}, nil
}

// parseTextOutput wraps parseSDKOutput for text-based results.
func parseTextOutput(text string, outputSchema json.RawMessage) (map[string]interface{}, int, bool) {
	return parseSDKOutput(&text, nil, outputSchema)
}

// registryToolsAsGoai converts MCP tools from iterion's tool.Registry into goai.Tool instances.
func registryToolsAsGoai(registry *tool.Registry) []goai.Tool {
	defs := registry.ListByOrigin(tool.OriginMCP)
	if len(defs) == 0 {
		return nil
	}

	tools := make([]goai.Tool, 0, len(defs))
	for _, def := range defs {
		tools = append(tools, goai.Tool{
			Name:        def.QualifiedName,
			Description: def.Description,
			InputSchema: def.InputSchema,
			Execute:     def.Execute,
		})
	}
	return tools
}
