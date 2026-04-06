package tool

import (
	"strings"

	goai "github.com/zendev-sh/goai"
)

// ---------------------------------------------------------------------------
// GoaiAdapter — bridge between ToolDef and goai.Tool
// ---------------------------------------------------------------------------

// sanitizedName returns the qualified name with dots replaced by underscores,
// safe for LLM APIs that restrict tool names to ^[a-zA-Z0-9_-]+$.
func (td *ToolDef) sanitizedName() string {
	return strings.ReplaceAll(td.QualifiedName, ".", "_")
}

// ToGoaiTool converts a ToolDef into a goai.Tool, which is the execution
// contract consumed by the GoaiExecutor. Both built-in and MCP tools
// produce the exact same goai.Tool shape. Tool names are sanitized
// (dots → underscores) for API compatibility.
func (td *ToolDef) ToGoaiTool() goai.Tool {
	return goai.Tool{
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
// goai.Tool slice. It stops at the first resolution error.
func (r *Registry) ResolveAll(refs []string) ([]goai.Tool, error) {
	tools := make([]goai.Tool, 0, len(refs))
	for _, ref := range refs {
		td, err := r.Resolve(ref)
		if err != nil {
			return nil, err
		}
		tools = append(tools, td.ToGoaiTool())
	}
	return tools, nil
}

// ResolveMap resolves a list of tool references and returns a map keyed
// by qualified name. Useful for the executor's tool lookup table.
func (r *Registry) ResolveMap(refs []string) (map[string]goai.Tool, error) {
	result := make(map[string]goai.Tool, len(refs))
	for _, ref := range refs {
		td, err := r.Resolve(ref)
		if err != nil {
			return nil, err
		}
		result[td.QualifiedName] = td.ToGoaiTool()
	}
	return result, nil
}
