package mcp

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"

	"github.com/SocialGouv/claw-code-go/internal/api"
)

// serverEntry holds a connected MCP client and its discovered tools.
type serverEntry struct {
	name      string
	client    *Client
	tools     []MCPTool
	resources []McpResourceInfo
}

// Registry manages connections to multiple MCP servers and their tools.
type Registry struct {
	servers []serverEntry
	mu      sync.RWMutex
}

// NewRegistry creates an empty Registry.
func NewRegistry() *Registry {
	return &Registry{}
}

// AddServer connects to an MCP server, runs the initialize handshake, and
// discovers its tools and resources. name is a human-readable label for the server.
func (r *Registry) AddServer(ctx context.Context, name string, transport Transport) error {
	client := NewClient(transport)

	if err := client.Initialize(ctx); err != nil {
		return fmt.Errorf("mcp registry: connect %q: %w", name, err)
	}

	tools, err := client.ListTools(ctx)
	if err != nil {
		return fmt.Errorf("mcp registry: list tools from %q: %w", name, err)
	}

	// Best-effort resource discovery — not all servers support resources.
	resources, resErr := client.ListResources(ctx)
	if resErr != nil {
		slog.Debug("best-effort ListResources failed", "server", name, "error", resErr)
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	r.servers = append(r.servers, serverEntry{
		name:      name,
		client:    client,
		tools:     tools,
		resources: resources,
	})

	return nil
}

// FindTool searches all registered servers for a tool with the given name.
// Returns the owning client, the tool definition, and whether it was found.
func (r *Registry) FindTool(toolName string) (*Client, *MCPTool, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	for i := range r.servers {
		for j := range r.servers[i].tools {
			if r.servers[i].tools[j].Name == toolName {
				return r.servers[i].client, &r.servers[i].tools[j], true
			}
		}
	}

	return nil, nil, false
}

// AllTools returns every tool from every connected server.
func (r *Registry) AllTools() []MCPTool {
	r.mu.RLock()
	defer r.mu.RUnlock()

	var result []MCPTool
	for _, s := range r.servers {
		result = append(result, s.tools...)
	}
	return result
}

// AllAPITools converts all MCP tools to the api.Tool format used by the model.
func (r *Registry) AllAPITools() []api.Tool {
	r.mu.RLock()
	defer r.mu.RUnlock()

	var result []api.Tool
	for _, s := range r.servers {
		for _, t := range s.tools {
			result = append(result, MCPToolToAPITool(t))
		}
	}
	return result
}

// AddServerFromConfig connects to an MCP server using the given transport
// configuration. It creates the transport via the shared transport factory,
// then delegates to AddServer for the handshake and tool discovery.
func (r *Registry) AddServerFromConfig(ctx context.Context, name string, cfg TransportConfig) error {
	transport, err := NewTransport(cfg)
	if err != nil {
		return fmt.Errorf("mcp registry: create transport for %q: %w", name, err)
	}
	if err := r.AddServer(ctx, name, transport); err != nil {
		// Close transport on handshake failure to avoid leaks.
		transport.Close()
		return err
	}
	return nil
}

// ServerNames returns the names of all registered servers.
func (r *Registry) ServerNames() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()

	names := make([]string, len(r.servers))
	for i, s := range r.servers {
		names[i] = s.name
	}
	return names
}

// ServerTools returns the tools for a named server, or nil if not found.
func (r *Registry) ServerTools(name string) []MCPTool {
	r.mu.RLock()
	defer r.mu.RUnlock()

	for _, s := range r.servers {
		if s.name == name {
			return s.tools
		}
	}
	return nil
}

// CallTool finds the server owning the named tool and calls it.
// This is the Registry-level active execution bridge (Rust parity).
func (r *Registry) CallTool(ctx context.Context, toolName string, arguments map[string]any) (MCPToolResult, error) {
	client, _, ok := r.FindTool(toolName)
	if !ok {
		return MCPToolResult{}, fmt.Errorf("mcp registry: tool %q not found on any server", toolName)
	}
	return client.CallTool(ctx, toolName, arguments)
}

// Disconnect closes and removes the named server.
func (r *Registry) Disconnect(name string) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	for i, s := range r.servers {
		if s.name == name {
			if err := s.client.Close(); err != nil {
				return fmt.Errorf("mcp registry: close %q: %w", name, err)
			}
			r.servers = append(r.servers[:i], r.servers[i+1:]...)
			return nil
		}
	}

	return fmt.Errorf("mcp registry: server %q not found", name)
}

// MCPToolToAPITool converts an MCP tool definition to the api.Tool format.
// Property types and descriptions are extracted on a best-effort basis.
func MCPToolToAPITool(t MCPTool) api.Tool {
	props := make(map[string]api.Property, len(t.InputSchema.Properties))
	for key, val := range t.InputSchema.Properties {
		prop := api.Property{}
		if m, ok := val.(map[string]any); ok {
			if typ, ok := m["type"].(string); ok {
				prop.Type = typ
			}
			if desc, ok := m["description"].(string); ok {
				prop.Description = desc
			}
		} else if s, ok := val.(string); ok {
			prop.Type = s
		}
		props[key] = prop
	}

	desc := t.Description
	if desc == "" {
		desc = fmt.Sprintf("Tool %q from MCP server", t.Name)
	}

	return api.Tool{
		Name:        t.Name,
		Description: desc,
		InputSchema: api.InputSchema{
			Type:       firstNonEmpty(t.InputSchema.Type, "object"),
			Properties: props,
			Required:   t.InputSchema.Required,
		},
	}
}

// GetServerInfo returns the server info string for a named server, or empty if not found.
// This is a narrow accessor matching Rust's server_info field on McpServerState.
func (r *Registry) GetServerInfo(name string) string {
	r.mu.RLock()
	defer r.mu.RUnlock()

	for _, s := range r.servers {
		if s.name == name {
			info := s.client.ServerInfo()
			if info.Name != "" {
				if info.Version != "" {
					return info.Name + " " + info.Version
				}
				return info.Name
			}
			return ""
		}
	}
	return ""
}

// GetResourceCount returns the number of resources for a named server.
// This is a narrow accessor matching Rust's resources.len() on McpServerState.
func (r *Registry) GetResourceCount(name string) int {
	r.mu.RLock()
	defer r.mu.RUnlock()

	for _, s := range r.servers {
		if s.name == name {
			return len(s.resources)
		}
	}
	return 0
}

// GetClient returns the MCP client for a named server, or nil if not found.
func (r *Registry) GetClient(name string) *Client {
	r.mu.RLock()
	defer r.mu.RUnlock()

	for _, s := range r.servers {
		if s.name == name {
			return s.client
		}
	}
	return nil
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}
