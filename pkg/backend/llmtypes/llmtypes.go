// Package llmtypes defines iterion-owned types for the LLM generation layer.
// These types decouple iterion's tool registry and model registry from any
// specific LLM SDK (claw-code-go, etc.), breaking what would otherwise
// be a circular dependency between model/ and tool/.
package llmtypes

import (
	"context"
	"encoding/json"
)

// LLMTool is an iterion-owned tool definition passed to the LLM generation
// layer. It decouples iterion from any SDK's tool shape — both claw-code-go's
// sdk.Tool and the existing tool.ToolDef bridge through this type.
type LLMTool struct {
	// Name is the tool's identifier (sanitized for LLM APIs).
	Name string

	// Description explains what the tool does.
	Description string

	// InputSchema is the JSON Schema for the tool's input parameters.
	InputSchema json.RawMessage

	// Execute runs the tool with the given JSON input and returns the result text.
	Execute func(ctx context.Context, input json.RawMessage) (string, error)
}

// FatalToolError is the interface for tool errors that should immediately
// stop the generation loop (e.g. rate limits, credit exhaustion).
// Implementations return true from IsFatal() to signal that the error
// should not be retried or absorbed by the LLM tool loop.
type FatalToolError interface {
	error
	IsFatal() bool
}

// ModelCapabilities describes what features a model supports.
// This is iterion-owned — decoupled from any SDK's capability type.
type ModelCapabilities struct {
	// Reasoning indicates the model supports extended thinking/reasoning.
	Reasoning bool

	// ToolCall indicates the model supports tool/function calling.
	ToolCall bool

	// Temperature indicates the model accepts a temperature parameter.
	Temperature bool
}
