package model

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	goai "github.com/zendev-sh/goai"
	"github.com/zendev-sh/goai/provider"

	"github.com/iterion-ai/iterion/ir"
)

// EventHooks allows the executor to emit observability events back to the caller.
type EventHooks struct {
	OnLLMRequest    func(nodeID string, info goai.RequestInfo)
	OnLLMResponse   func(nodeID string, info goai.ResponseInfo)
	OnLLMStepFinish func(nodeID string, step goai.StepResult)
	OnToolCall      func(nodeID string, info goai.ToolCallInfo)
}

// GoaiExecutor implements runtime.NodeExecutor by delegating LLM calls
// to goai's GenerateText and GenerateObject APIs.
type GoaiExecutor struct {
	registry *Registry
	prompts  map[string]*ir.Prompt
	schemas  map[string]*ir.Schema
	vars     map[string]interface{}
	tools    map[string]goai.Tool // registered tool implementations
	hooks    EventHooks
}

// GoaiExecutorOption configures a GoaiExecutor.
type GoaiExecutorOption func(*GoaiExecutor)

// WithEventHooks sets observability callbacks on the executor.
func WithEventHooks(h EventHooks) GoaiExecutorOption {
	return func(e *GoaiExecutor) { e.hooks = h }
}

// WithToolImplementations registers tool implementations by name.
func WithToolImplementations(tools map[string]goai.Tool) GoaiExecutorOption {
	return func(e *GoaiExecutor) { e.tools = tools }
}

// NewGoaiExecutor creates a GoaiExecutor for a given workflow.
func NewGoaiExecutor(registry *Registry, wf *ir.Workflow, opts ...GoaiExecutorOption) *GoaiExecutor {
	e := &GoaiExecutor{
		registry: registry,
		prompts:  wf.Prompts,
		schemas:  wf.Schemas,
		tools:    make(map[string]goai.Tool),
	}
	for _, opt := range opts {
		opt(e)
	}
	return e
}

// SetVars sets the workflow variables for the current run.
// Must be called before Execute.
func (e *GoaiExecutor) SetVars(vars map[string]interface{}) {
	e.vars = vars
}

// Execute implements runtime.NodeExecutor.
func (e *GoaiExecutor) Execute(ctx context.Context, node *ir.Node, input map[string]interface{}) (map[string]interface{}, error) {
	switch node.Kind {
	case ir.NodeAgent, ir.NodeJudge:
		return e.executeLLM(ctx, node, input)
	case ir.NodeRouter:
		// Routers are deterministic pass-throughs handled by the engine.
		return input, nil
	case ir.NodeTool:
		return e.executeToolNode(ctx, node, input)
	default:
		return nil, fmt.Errorf("model: unsupported node kind %q for execution", node.Kind)
	}
}

// executeLLM handles agent and judge nodes by calling goai.
func (e *GoaiExecutor) executeLLM(ctx context.Context, node *ir.Node, input map[string]interface{}) (map[string]interface{}, error) {
	// Resolve model.
	m, err := e.registry.Resolve(node.Model)
	if err != nil {
		return nil, fmt.Errorf("model: node %q: %w", node.ID, err)
	}

	// Build goai options.
	var opts []goai.Option

	// System prompt.
	if node.SystemPrompt != "" {
		if p, ok := e.prompts[node.SystemPrompt]; ok {
			systemText := e.resolveTemplate(p.Body, input)
			opts = append(opts, goai.WithSystem(systemText))
		}
	}

	// User message from user prompt or input.
	userText := e.buildUserMessage(node, input)
	if userText != "" {
		opts = append(opts, goai.WithMessages(goai.UserMessage(userText)))
	}

	// Tools.
	if len(node.Tools) > 0 {
		var tools []goai.Tool
		for _, toolName := range node.Tools {
			if t, ok := e.tools[toolName]; ok {
				tools = append(tools, t)
			}
		}
		if len(tools) > 0 {
			opts = append(opts, goai.WithTools(tools...))
			maxSteps := node.ToolMaxSteps
			if maxSteps <= 0 {
				maxSteps = 5
			}
			opts = append(opts, goai.WithMaxSteps(maxSteps))
		}
	}

	// Observability hooks.
	nodeID := node.ID
	if e.hooks.OnLLMRequest != nil {
		fn := e.hooks.OnLLMRequest
		opts = append(opts, goai.WithOnRequest(func(info goai.RequestInfo) {
			fn(nodeID, info)
		}))
	}
	if e.hooks.OnLLMResponse != nil {
		fn := e.hooks.OnLLMResponse
		opts = append(opts, goai.WithOnResponse(func(info goai.ResponseInfo) {
			fn(nodeID, info)
		}))
	}
	if e.hooks.OnLLMStepFinish != nil {
		fn := e.hooks.OnLLMStepFinish
		opts = append(opts, goai.WithOnStepFinish(func(step goai.StepResult) {
			fn(nodeID, step)
		}))
	}
	if e.hooks.OnToolCall != nil {
		fn := e.hooks.OnToolCall
		opts = append(opts, goai.WithOnToolCall(func(info goai.ToolCallInfo) {
			fn(nodeID, info)
		}))
	}

	// Structured output (output schema present).
	if node.OutputSchema != "" {
		return e.generateStructured(ctx, m, node, opts)
	}

	// Text generation.
	return e.generateText(ctx, m, node, opts)
}

// generateStructured uses goai.GenerateObject with an explicit JSON schema.
func (e *GoaiExecutor) generateStructured(ctx context.Context, m provider.LanguageModel, node *ir.Node, opts []goai.Option) (map[string]interface{}, error) {
	schema, ok := e.schemas[node.OutputSchema]
	if !ok {
		return nil, fmt.Errorf("model: node %q references unknown schema %q", node.ID, node.OutputSchema)
	}

	jsonSchema, err := SchemaToJSON(schema)
	if err != nil {
		return nil, fmt.Errorf("model: node %q: schema conversion: %w", node.ID, err)
	}

	opts = append(opts, goai.WithExplicitSchema(jsonSchema))

	result, err := goai.GenerateObject[map[string]interface{}](ctx, m, opts...)
	if err != nil {
		return nil, fmt.Errorf("model: node %q: structured generation: %w", node.ID, err)
	}

	output := result.Object
	if output == nil {
		output = make(map[string]interface{})
	}

	// Attach usage metadata.
	output["_tokens"] = result.Usage.InputTokens + result.Usage.OutputTokens
	output["_model"] = m.ModelID()

	return output, nil
}

// generateText uses goai.GenerateText for free-form text output.
func (e *GoaiExecutor) generateText(ctx context.Context, m provider.LanguageModel, node *ir.Node, opts []goai.Option) (map[string]interface{}, error) {
	result, err := goai.GenerateText(ctx, m, opts...)
	if err != nil {
		return nil, fmt.Errorf("model: node %q: text generation: %w", node.ID, err)
	}

	output := map[string]interface{}{
		"text":    result.Text,
		"_tokens": result.TotalUsage.InputTokens + result.TotalUsage.OutputTokens,
		"_model":  m.ModelID(),
	}

	return output, nil
}

// executeToolNode runs a tool node (direct command, no LLM).
func (e *GoaiExecutor) executeToolNode(ctx context.Context, node *ir.Node, input map[string]interface{}) (map[string]interface{}, error) {
	toolName := node.Command
	tool, ok := e.tools[toolName]
	if !ok {
		return nil, fmt.Errorf("model: tool node %q references unregistered tool %q", node.ID, toolName)
	}

	inputJSON, err := json.Marshal(input)
	if err != nil {
		return nil, fmt.Errorf("model: tool node %q: marshal input: %w", node.ID, err)
	}

	start := time.Now()
	outputStr, err := tool.Execute(ctx, inputJSON)
	if e.hooks.OnToolCall != nil {
		e.hooks.OnToolCall(node.ID, goai.ToolCallInfo{
			ToolName:  toolName,
			InputSize: len(inputJSON),
			Duration:  time.Since(start),
			Error:     err,
		})
	}
	if err != nil {
		return nil, fmt.Errorf("model: tool node %q: execute: %w", node.ID, err)
	}

	// Try to parse tool output as JSON map, otherwise wrap as text.
	var output map[string]interface{}
	if jsonErr := json.Unmarshal([]byte(outputStr), &output); jsonErr != nil {
		output = map[string]interface{}{"result": outputStr}
	}

	return output, nil
}

// buildUserMessage constructs the user message for an LLM call.
func (e *GoaiExecutor) buildUserMessage(node *ir.Node, input map[string]interface{}) string {
	// If the node has a user prompt template, resolve it.
	if node.UserPrompt != "" {
		if p, ok := e.prompts[node.UserPrompt]; ok {
			return e.resolveTemplate(p.Body, input)
		}
	}

	// Fallback: serialize input as the user message.
	if len(input) == 0 {
		return ""
	}

	b, err := json.Marshal(input)
	if err != nil {
		return fmt.Sprintf("%v", input)
	}
	return string(b)
}

// resolveTemplate substitutes {{...}} references in a prompt body.
func (e *GoaiExecutor) resolveTemplate(body string, input map[string]interface{}) string {
	var b strings.Builder
	remaining := body

	for {
		start := strings.Index(remaining, "{{")
		if start == -1 {
			b.WriteString(remaining)
			break
		}
		end := strings.Index(remaining[start:], "}}")
		if end == -1 {
			b.WriteString(remaining)
			break
		}
		end += start + 2

		b.WriteString(remaining[:start])

		ref := strings.TrimSpace(remaining[start+2 : end-2])
		val, resolved := e.resolveTemplateRef(ref, input)
		if resolved {
			b.WriteString(val)
		} else {
			// Keep unresolved refs as-is.
			b.WriteString(remaining[start:end])
		}

		remaining = remaining[end:]
	}

	return b.String()
}

// resolveTemplateRef resolves a single "namespace.path" reference.
// Returns the resolved value and true, or ("", false) if unresolvable.
func (e *GoaiExecutor) resolveTemplateRef(ref string, input map[string]interface{}) (string, bool) {
	parts := strings.SplitN(ref, ".", 2)
	if len(parts) < 2 {
		return "", false
	}

	namespace := parts[0]
	key := parts[1]

	switch namespace {
	case "input":
		if v, ok := input[key]; ok {
			return formatValue(v), true
		}
	case "vars":
		if e.vars != nil {
			if v, ok := e.vars[key]; ok {
				return formatValue(v), true
			}
		}
	}

	return "", false
}

// formatValue converts an interface value to a string for template substitution.
func formatValue(v interface{}) string {
	switch val := v.(type) {
	case string:
		return val
	case nil:
		return ""
	default:
		b, err := json.Marshal(val)
		if err != nil {
			return fmt.Sprintf("%v", val)
		}
		return string(b)
	}
}
