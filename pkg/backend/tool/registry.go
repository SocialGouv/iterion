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
	"sort"
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

// Unregister removes a tool by its qualified name. No-op if the name
// isn't currently registered, so callers performing rollback after a
// partial RegisterMCP loop don't need to track which subset actually
// landed.
func (r *Registry) Unregister(qualifiedName string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.tools, qualifiedName)
}

// UnregisterServer removes every MCP tool registered under the given
// server name. Used by Manager.EnsureServers to roll back partial
// discovery — when half the configured servers register cleanly and a
// later one fails, leaving the half-registered subset live would let
// a workflow run with an unexpectedly truncated tool catalog.
func (r *Registry) UnregisterServer(server string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for name, td := range r.tools {
		if td.Origin.Kind == OriginMCP && td.Origin.Server == server {
			delete(r.tools, name)
		}
	}
}

// ---------------------------------------------------------------------------
// Resolution
// ---------------------------------------------------------------------------

// Resolve looks up a tool by its reference name. Resolution rules:
//  1. Exact match on qualified name (dot-delimited: "mcp.server.tool").
//  2. claude_code-style FQN match: "mcp__server__tool" is rewritten to
//     "mcp.server.tool" and re-resolved. Lets a prompt written for the
//     claude_code backend (which surfaces MCP tools under the
//     double-underscore FQN) run unchanged on the claw backend (which
//     registers them under the dot form).
//  3. If ref has no dots / double-underscores and there is exactly one
//     MCP tool whose bare name matches, return it (convenience
//     shorthand). If multiple MCP tools share the same bare name,
//     return an ambiguity error.
//
// Returns an error if the tool is not found or is ambiguous.
func (r *Registry) Resolve(ref string) (*ToolDef, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	// Exact match.
	if td, ok := r.tools[ref]; ok {
		return td, nil
	}

	// claude_code FQN compatibility: "mcp__server__tool" → "mcp.server.tool".
	if strings.HasPrefix(ref, "mcp__") {
		// Strip the leading "mcp__" then split the rest into
		// server/tool segments on "__". A well-formed FQN has
		// exactly one inner "__" (server / tool); accept more by
		// taking everything before the LAST "__" as the server.
		// This keeps tool names with embedded underscores intact.
		body := ref[len("mcp__"):]
		idx := strings.LastIndex(body, "__")
		if idx > 0 {
			server := body[:idx]
			tool := body[idx+2:]
			if server != "" && tool != "" {
				if td, ok := r.tools["mcp."+server+"."+tool]; ok {
					return td, nil
				}
			}
		}
	}

	// Shorthand resolution: bare name → scan MCP tools.
	if !strings.Contains(ref, ".") && !strings.Contains(ref, "__") {
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
			sort.Strings(names)
			return nil, fmt.Errorf("tool: ambiguous reference %q matches multiple tools: %s", ref, strings.Join(names, ", "))
		}
	}

	return nil, fmt.Errorf("tool: unknown tool %q", ref)
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

// ListByServer returns tools from a specific MCP server.
func (r *Registry) ListByServer(server string) []*ToolDef {
	r.mu.RLock()
	defer r.mu.RUnlock()

	var result []*ToolDef
	for _, td := range r.tools {
		if td.Origin.Kind == OriginMCP && td.Origin.Server == server {
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

// IsMCPWildcard returns true if name matches the MCP server wildcard pattern
// "mcp.<server>.*".
func IsMCPWildcard(name string) bool {
	_, err := ParseMCPWildcard(name)
	return err == nil
}

// ParseMCPWildcard extracts the server name from a wildcard pattern
// "mcp.<server>.*". Returns an error if the format is invalid.
func ParseMCPWildcard(name string) (server string, err error) {
	// Minimum valid: "mcp.X.*" = 7 chars.
	if len(name) < 7 || !strings.HasPrefix(name, "mcp.") || !strings.HasSuffix(name, ".*") {
		return "", fmt.Errorf("tool: %q is not an MCP wildcard (expected \"mcp.<server>.*\")", name)
	}
	server = name[4 : len(name)-2] // strip "mcp." prefix and ".*" suffix
	if server == "" || strings.Contains(server, ".") {
		return "", fmt.Errorf("tool: invalid MCP wildcard %q (server name must be non-empty and contain no dots)", name)
	}
	return server, nil
}
