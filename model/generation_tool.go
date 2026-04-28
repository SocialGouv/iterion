package model

import (
	"context"
	"encoding/json"

	"github.com/SocialGouv/claw-code-go/pkg/api"
)

// GenerationTool describes a tool available during direct generation.
// It replaces sdk.Tool for the native generation engine.
type GenerationTool struct {
	// Name is the tool identifier.
	Name string

	// Description describes what the tool does.
	Description string

	// InputSchema is the JSON schema for the tool's input parameters.
	InputSchema json.RawMessage

	// Execute runs the tool with the given JSON input and returns the result text.
	Execute func(ctx context.Context, input json.RawMessage) (string, error)
}

// GenerationOptions configures a direct generation call.
type GenerationOptions struct {
	// Model is the model ID (e.g., "claude-sonnet-4-6").
	Model string

	// System is the system prompt.
	System string

	// Messages is the initial conversation history.
	Messages []api.Message

	// Tools available for the model to call.
	Tools []GenerationTool

	// MaxSteps is the maximum number of tool-loop iterations (default 10).
	MaxSteps int

	// MaxTokens is the maximum tokens per response (default 8192).
	MaxTokens int

	// Temperature controls randomness (nil = provider default).
	Temperature *float64

	// ExplicitSchema is the JSON schema for structured output (GenerateObjectDirect).
	ExplicitSchema json.RawMessage

	// SchemaName is the name for the synthetic structured-output tool (default "structured_output").
	SchemaName string

	// ProviderOptions are provider-specific options (e.g., reasoning_effort).
	ProviderOptions map[string]any

	// --- Hook callbacks ---

	// OnRequest is called before each StreamResponse call.
	OnRequest func(RequestInfo)

	// OnResponse is called after each StreamResponse aggregation completes.
	OnResponse func(ResponseInfo)

	// OnStepFinish is called after each tool-loop step completes.
	OnStepFinish func(StepResult)

	// OnToolCall is called after each tool execution.
	OnToolCall func(ToolCallInfo)
}
