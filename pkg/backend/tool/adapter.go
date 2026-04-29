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

// ---------------------------------------------------------------------------
// Batch helpers
// ---------------------------------------------------------------------------

// ResolveAll resolves a list of tool references and returns the corresponding
// llmtypes.LLMTool slice. It stops at the first resolution error.
func (r *Registry) ResolveAll(refs []string) ([]llmtypes.LLMTool, error) {
	tools := make([]llmtypes.LLMTool, 0, len(refs))
	for _, ref := range refs {
		td, err := r.Resolve(ref)
		if err != nil {
			return nil, err
		}
		tools = append(tools, td.ToLLMTool())
	}
	return tools, nil
}

// ResolveMap resolves a list of tool references and returns a map keyed
// by qualified name. Useful for the executor's tool lookup table.
func (r *Registry) ResolveMap(refs []string) (map[string]llmtypes.LLMTool, error) {
	result := make(map[string]llmtypes.LLMTool, len(refs))
	for _, ref := range refs {
		td, err := r.Resolve(ref)
		if err != nil {
			return nil, err
		}
		result[td.QualifiedName] = td.ToLLMTool()
	}
	return result, nil
}
