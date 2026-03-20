// Package tool provides a unified ToolRegistry that normalizes built-in
// tools and MCP server tools under a single namespace and resolution scheme.
//
// Built-in tools are registered under their bare name (e.g. "git_diff").
// MCP tools follow the namespace convention "mcp.<server>.<tool>".
// Workflows reference tools by their qualified name; the registry resolves
// them unambiguously and detects collisions at registration time.
package tool

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
)

// ---------------------------------------------------------------------------
// ToolDef — unified tool definition
// ---------------------------------------------------------------------------

// ToolDef is the registry's canonical tool representation. Both built-in
// and MCP tools are stored as ToolDefs.
type ToolDef struct {
	// QualifiedName is the unique key in the registry.
	// Built-ins: "git_diff"
	// MCP tools: "mcp.github.create_issue"
	QualifiedName string

	// Description explains what the tool does.
	Description string

	// InputSchema is the JSON Schema for the tool's input parameters.
	InputSchema json.RawMessage

	// Execute runs the tool with the given JSON input.
	Execute func(ctx context.Context, input json.RawMessage) (string, error)

	// Origin tracks where this tool came from.
	Origin Origin
}

// Origin describes the provenance of a tool.
type Origin struct {
	Kind   OriginKind // builtin or mcp
	Server string     // MCP server name (empty for built-ins)
}

// OriginKind discriminates built-in vs MCP tools.
type OriginKind int

const (
	OriginBuiltin OriginKind = iota
	OriginMCP
)

func (ok OriginKind) String() string {
	switch ok {
	case OriginBuiltin:
		return "builtin"
	case OriginMCP:
		return "mcp"
	default:
		return "unknown"
	}
}

// ---------------------------------------------------------------------------
// Registry
// ---------------------------------------------------------------------------

// Registry is a thread-safe store for tool definitions. It enforces unique
// qualified names and provides resolution by name.
type Registry struct {
	mu    sync.RWMutex
	tools map[string]*ToolDef // qualifiedName → tool
}

// NewRegistry creates an empty tool registry.
func NewRegistry() *Registry {
	return &Registry{
		tools: make(map[string]*ToolDef),
	}
}

// ---------------------------------------------------------------------------
// Registration
// ---------------------------------------------------------------------------

// RegisterBuiltin adds a built-in tool. The name must not contain dots.
// Returns an error if the name collides with an existing registration.
func (r *Registry) RegisterBuiltin(name string, desc string, schema json.RawMessage, exec func(ctx context.Context, input json.RawMessage) (string, error)) error {
	if strings.Contains(name, ".") {
		return fmt.Errorf("tool: builtin name %q must not contain dots", name)
	}
	if name == "" {
		return fmt.Errorf("tool: builtin name must not be empty")
	}

	td := &ToolDef{
		QualifiedName: name,
		Description:   desc,
		InputSchema:   schema,
		Execute:       exec,
		Origin:        Origin{Kind: OriginBuiltin},
	}
	return r.register(td)
}

// RegisterMCP adds an MCP tool with the qualified name "mcp.<server>.<tool>".
// Both server and toolName must be non-empty and must not contain dots.
// Returns an error on collision.
func (r *Registry) RegisterMCP(server, toolName, desc string, schema json.RawMessage, exec func(ctx context.Context, input json.RawMessage) (string, error)) error {
	if server == "" {
		return fmt.Errorf("tool: MCP server name must not be empty")
	}
	if toolName == "" {
		return fmt.Errorf("tool: MCP tool name must not be empty")
	}
	if strings.Contains(server, ".") {
		return fmt.Errorf("tool: MCP server name %q must not contain dots", server)
	}
	if strings.Contains(toolName, ".") {
		return fmt.Errorf("tool: MCP tool name %q must not contain dots", toolName)
	}

	qualified := "mcp." + server + "." + toolName
	td := &ToolDef{
		QualifiedName: qualified,
		Description:   desc,
		InputSchema:   schema,
		Execute:       exec,
		Origin:        Origin{Kind: OriginMCP, Server: server},
	}
	return r.register(td)
}

// register adds a ToolDef to the registry, returning an error on collision.
func (r *Registry) register(td *ToolDef) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	if existing, ok := r.tools[td.QualifiedName]; ok {
		return fmt.Errorf("tool: name collision — %q already registered (origin: %s)", td.QualifiedName, existing.Origin.Kind)
	}
	r.tools[td.QualifiedName] = td
	return nil
}

// ---------------------------------------------------------------------------
// Resolution
// ---------------------------------------------------------------------------

// Resolve looks up a tool by its reference name. Resolution rules:
//  1. Exact match on qualified name.
//  2. If ref has no dots and there is exactly one MCP tool whose bare name
//     matches, return it (convenience shorthand). If multiple MCP tools
//     share the same bare name, return an ambiguity error.
//
// Returns an error if the tool is not found or is ambiguous.
func (r *Registry) Resolve(ref string) (*ToolDef, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	// Exact match.
	if td, ok := r.tools[ref]; ok {
		return td, nil
	}

	// Shorthand resolution: bare name → scan MCP tools.
	if !strings.Contains(ref, ".") {
		var matches []*ToolDef
		suffix := "." + ref
		for qn, td := range r.tools {
			if strings.HasSuffix(qn, suffix) && td.Origin.Kind == OriginMCP {
				matches = append(matches, td)
			}
		}
		switch len(matches) {
		case 1:
			return matches[0], nil
		case 0:
			// fall through to not-found
		default:
			names := make([]string, len(matches))
			for i, m := range matches {
				names[i] = m.QualifiedName
			}
			return nil, fmt.Errorf("tool: ambiguous reference %q matches multiple tools: %s", ref, strings.Join(names, ", "))
		}
	}

	return nil, fmt.Errorf("tool: unknown tool %q", ref)
}

// MustResolve is like Resolve but panics on error. Intended for tests.
func (r *Registry) MustResolve(ref string) *ToolDef {
	td, err := r.Resolve(ref)
	if err != nil {
		panic(err)
	}
	return td
}

// ---------------------------------------------------------------------------
// Listing
// ---------------------------------------------------------------------------

// List returns all registered tools. The order is not guaranteed.
func (r *Registry) List() []*ToolDef {
	r.mu.RLock()
	defer r.mu.RUnlock()

	result := make([]*ToolDef, 0, len(r.tools))
	for _, td := range r.tools {
		result = append(result, td)
	}
	return result
}

// ListByOrigin returns tools filtered by origin kind.
func (r *Registry) ListByOrigin(kind OriginKind) []*ToolDef {
	r.mu.RLock()
	defer r.mu.RUnlock()

	var result []*ToolDef
	for _, td := range r.tools {
		if td.Origin.Kind == kind {
			result = append(result, td)
		}
	}
	return result
}

// Len returns the number of registered tools.
func (r *Registry) Len() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.tools)
}

// ---------------------------------------------------------------------------
// Namespace helpers
// ---------------------------------------------------------------------------

// ParseMCPName splits "mcp.<server>.<tool>" into server and tool name.
// Returns an error if the format is invalid.
func ParseMCPName(qualified string) (server, toolName string, err error) {
	if !strings.HasPrefix(qualified, "mcp.") {
		return "", "", fmt.Errorf("tool: %q is not an MCP qualified name (must start with \"mcp.\")", qualified)
	}
	rest := qualified[4:] // strip "mcp."
	idx := strings.Index(rest, ".")
	if idx <= 0 || idx == len(rest)-1 {
		return "", "", fmt.Errorf("tool: invalid MCP qualified name %q (expected \"mcp.<server>.<tool>\")", qualified)
	}
	return rest[:idx], rest[idx+1:], nil
}

// IsMCPName returns true if the qualified name follows the MCP convention.
func IsMCPName(name string) bool {
	_, _, err := ParseMCPName(name)
	return err == nil
}
