package tools

import (
	"github.com/SocialGouv/claw-code-go/internal/api"
	"github.com/SocialGouv/claw-code-go/internal/mcp"
	"context"
	"encoding/json"
	"fmt"
)

// --- MCP resource/auth tool definitions ---

func ListMcpResourcesTool() api.Tool {
	return api.Tool{
		Name:        "list_mcp_resources",
		Description: "List resources available on an MCP server.",
		InputSchema: api.InputSchema{
			Type: "object",
			Properties: map[string]api.Property{
				"server": {Type: "string", Description: "Server name (defaults to 'default')."},
			},
		},
	}
}

func ReadMcpResourceTool() api.Tool {
	return api.Tool{
		Name:        "read_mcp_resource",
		Description: "Read a specific resource from an MCP server.",
		InputSchema: api.InputSchema{
			Type: "object",
			Properties: map[string]api.Property{
				"server": {Type: "string", Description: "Server name (defaults to 'default')."},
				"uri":    {Type: "string", Description: "Resource URI to read."},
			},
			Required: []string{"uri"},
		},
	}
}

func McpAuthTool() api.Tool {
	return api.Tool{
		Name:        "mcp_auth",
		Description: "Check the authentication and connection status of an MCP server.",
		InputSchema: api.InputSchema{
			Type: "object",
			Properties: map[string]api.Property{
				"server": {Type: "string", Description: "Server name to check."},
			},
			Required: []string{"server"},
		},
	}
}

// --- MCP resource/auth Execute functions ---

func ExecuteListMcpResources(input map[string]any, registry *mcp.Registry) (string, error) {
	if registry == nil {
		return "", fmt.Errorf("list_mcp_resources: MCP registry not available")
	}

	server := "default"
	if s, ok := input["server"].(string); ok && s != "" {
		server = s
	}

	client, ok := findServerClient(registry, server)
	if !ok {
		result := map[string]any{
			"server":    server,
			"resources": []any{},
			"error":     fmt.Sprintf("server %q not found", server),
		}
		out, _ := json.MarshalIndent(result, "", "  ")
		return string(out), nil
	}

	resources, err := client.ListResources(context.Background())
	if err != nil {
		result := map[string]any{
			"server":    server,
			"resources": []any{},
			"error":     err.Error(),
		}
		out, _ := json.MarshalIndent(result, "", "  ")
		return string(out), nil
	}

	result := map[string]any{
		"server":    server,
		"resources": resources,
		"count":     len(resources),
	}
	out, _ := json.MarshalIndent(result, "", "  ")
	return string(out), nil
}

func ExecuteReadMcpResource(input map[string]any, registry *mcp.Registry) (string, error) {
	if registry == nil {
		return "", fmt.Errorf("read_mcp_resource: MCP registry not available")
	}

	uri, ok := input["uri"].(string)
	if !ok || uri == "" {
		return "", fmt.Errorf("read_mcp_resource: 'uri' is required")
	}

	server := "default"
	if s, ok := input["server"].(string); ok && s != "" {
		server = s
	}

	client, ok := findServerClient(registry, server)
	if !ok {
		result := map[string]any{
			"server": server,
			"uri":    uri,
			"error":  fmt.Sprintf("server %q not found", server),
		}
		out, _ := json.MarshalIndent(result, "", "  ")
		return string(out), nil
	}

	resource, err := client.ReadResource(context.Background(), uri)
	if err != nil {
		result := map[string]any{
			"server": server,
			"uri":    uri,
			"error":  err.Error(),
		}
		out, _ := json.MarshalIndent(result, "", "  ")
		return string(out), nil
	}

	// Match Rust response shape: {server, uri, name, description, mime_type}.
	// Content is available internally via McpResourceContent but excluded from tool output.
	result := map[string]any{
		"server":      server,
		"uri":         resource.URI,
		"name":        resource.Name,
		"description": resource.Description,
		"mime_type":   resource.MimeType,
	}
	out, _ := json.MarshalIndent(result, "", "  ")
	return string(out), nil
}

func ExecuteMcpAuth(input map[string]any, registry *mcp.Registry, authState *mcp.AuthState) (string, error) {
	if registry == nil {
		return "", fmt.Errorf("mcp_auth: MCP registry not available")
	}

	server, ok := input["server"].(string)
	if !ok || server == "" {
		return "", fmt.Errorf("mcp_auth: 'server' is required")
	}

	// Check if server is registered.
	serverNames := registry.ServerNames()
	found := false
	for _, name := range serverNames {
		if name == server {
			found = true
			break
		}
	}

	if !found {
		result := map[string]any{
			"server":  server,
			"status":  "disconnected",
			"message": "Server not registered. Use MCP tool to connect first.",
		}
		out, _ := json.MarshalIndent(result, "", "  ")
		return string(out), nil
	}

	// Get connection status from auth state.
	var status string
	if authState != nil {
		status = authState.GetStatus(server).String()
	} else {
		status = "connected" // If no auth state, assume connected since server is registered.
	}

	// Match Rust response shape: {server, status, server_info, tool_count, resource_count}
	serverInfo := registry.GetServerInfo(server)
	resourceCount := registry.GetResourceCount(server)

	result := map[string]any{
		"server":         server,
		"status":         status,
		"server_info":    serverInfo,
		"tool_count":     len(registry.ServerTools(server)),
		"resource_count": resourceCount,
	}
	out, _ := json.MarshalIndent(result, "", "  ")
	return string(out), nil
}

// findServerClient looks up the MCP client for a named server.
func findServerClient(registry *mcp.Registry, serverName string) (*mcp.Client, bool) {
	client := registry.GetClient(serverName)
	return client, client != nil
}
