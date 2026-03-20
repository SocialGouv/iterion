package tool

import (
	"context"
	"encoding/json"

	goai "github.com/zendev-sh/goai"
)

// ---------------------------------------------------------------------------
// GoaiAdapter — bridge between ToolDef and goai.Tool
// ---------------------------------------------------------------------------

// ToGoaiTool converts a ToolDef into a goai.Tool, which is the execution
// contract consumed by the GoaiExecutor. Both built-in and MCP tools
// produce the exact same goai.Tool shape.
func (td *ToolDef) ToGoaiTool() goai.Tool {
	return goai.Tool{
		Name:        td.QualifiedName,
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

// ---------------------------------------------------------------------------
// Convenience builders
// ---------------------------------------------------------------------------

// NewBuiltinDef creates a ToolDef for a built-in tool. This is a lower-level
// helper; prefer RegisterBuiltin for direct registration.
func NewBuiltinDef(name, desc string, schema json.RawMessage, exec func(ctx context.Context, input json.RawMessage) (string, error)) *ToolDef {
	return &ToolDef{
		QualifiedName: name,
		Description:   desc,
		InputSchema:   schema,
		Execute:       exec,
		Origin:        Origin{Kind: OriginBuiltin},
	}
}

// NewMCPDef creates a ToolDef for an MCP tool with the qualified name
// "mcp.<server>.<tool>". This is a lower-level helper; prefer RegisterMCP
// for direct registration.
func NewMCPDef(server, toolName, desc string, schema json.RawMessage, exec func(ctx context.Context, input json.RawMessage) (string, error)) *ToolDef {
	return &ToolDef{
		QualifiedName: "mcp." + server + "." + toolName,
		Description:   desc,
		InputSchema:   schema,
		Execute:       exec,
		Origin:        Origin{Kind: OriginMCP, Server: server},
	}
}
