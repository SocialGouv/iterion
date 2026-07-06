package tool

import (
	"strings"

	"github.com/SocialGouv/iterion/pkg/backend/delegate"
	"github.com/SocialGouv/iterion/pkg/backend/llmtypes"
)

// ---------------------------------------------------------------------------
// LLMTool adapter — bridge between ToolDef and llmtypes.LLMTool
// ---------------------------------------------------------------------------

// sanitizedName returns the qualified name with dots replaced by underscores,
// safe for LLM APIs that restrict tool names to ^[a-zA-Z0-9_-]+$.
func (td *ToolDef) sanitizedName() string {
	return strings.ReplaceAll(td.QualifiedName, ".", "_")
}

// ToLLMTool converts a ToolDef into an llmtypes.LLMTool, which is the execution
// contract consumed by the LLM generation layer. Both built-in and MCP tools
// produce the exact same LLMTool shape. Tool names are sanitized
// (dots → underscores) for API compatibility.
func (td *ToolDef) ToLLMTool() llmtypes.LLMTool {
	return llmtypes.LLMTool{
		Name:        td.sanitizedName(),
		Description: td.Description,
		InputSchema: td.InputSchema,
		Execute:     td.Execute,
	}
}

// ToDelegateDef converts a ToolDef into a delegate.ToolDef, which is the
// execution contract consumed by the backend dispatch layer.
func (td *ToolDef) ToDelegateDef() delegate.ToolDef {
	return delegate.ToolDef{
		Name:        td.sanitizedName(),
		Description: td.Description,
		InputSchema: td.InputSchema,
		Execute:     td.Execute,
	}
}
