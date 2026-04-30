package tools

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/SocialGouv/claw-code-go/internal/api"
	"github.com/SocialGouv/claw-code-go/internal/mcp"
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

func ExecuteListMcpResources(ctx context.Context, input map[string]any, provider mcp.Provider) (string, error) {
	if provider == nil {
		return "", fmt.Errorf("list_mcp_resources: MCP provider not available")
	}

	server := "default"
	if s, ok := input["server"].(string); ok && s != "" {
		server = s
	}

	client, ok := provider.GetResourceClient(server)
	if !ok {
		result := map[string]any{
			"server":    server,
			"resources": []any{},
			"error":     fmt.Sprintf("server %q not found", server),
		}
		out, _ := json.MarshalIndent(result, "", "  ")
		return string(out), nil
	}

	resources, err := client.ListResources(ctx)
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

func ExecuteReadMcpResource(ctx context.Context, input map[string]any, provider mcp.Provider) (string, error) {
	if provider == nil {
		return "", fmt.Errorf("read_mcp_resource: MCP provider not available")
	}

	uri, ok := input["uri"].(string)
	if !ok || uri == "" {
		return "", fmt.Errorf("read_mcp_resource: 'uri' is required")
	}

	server := "default"
	if s, ok := input["server"].(string); ok && s != "" {
		server = s
	}

	client, ok := provider.GetResourceClient(server)
	if !ok {
		result := map[string]any{
			"server": server,
			"uri":    uri,
			"error":  fmt.Sprintf("server %q not found", server),
		}
		out, _ := json.MarshalIndent(result, "", "  ")
		return string(out), nil
	}

	resource, err := client.ReadResource(ctx, uri)
	if err != nil {
		result := map[string]any{
			"server": server,
			"uri":    uri,
			"error":  err.Error(),
		}
		out, _ := json.MarshalIndent(result, "", "  ")
		return string(out), nil
	}

	// Response shape: {server, uri, name, description, mime_type, content}.
	// `content` is the resource body (text); kept here so callers that need
	// the body don't have to issue a second round-trip.
	result := map[string]any{
		"server":      server,
		"uri":         resource.URI,
		"name":        resource.Name,
		"description": resource.Description,
		"mime_type":   resource.MimeType,
		"content":     resource.Content,
	}
	out, _ := json.MarshalIndent(result, "", "  ")
	return string(out), nil
}

func ExecuteMcpAuth(ctx context.Context, input map[string]any, provider mcp.Provider) (string, error) {
	if provider == nil {
		return "", fmt.Errorf("mcp_auth: MCP provider not available")
	}

	server, ok := input["server"].(string)
	if !ok || server == "" {
		return "", fmt.Errorf("mcp_auth: 'server' is required")
	}

	status, ok := provider.ServerStatus(server)
	if !ok {
		result := map[string]any{
			"server":  server,
			"status":  "disconnected",
			"message": "Server not registered. Use MCP tool to connect first.",
		}
		out, _ := json.MarshalIndent(result, "", "  ")
		return string(out), nil
	}

	// Match Rust response shape: {server, status, server_info, tool_count, resource_count}.
	result := map[string]any{
		"server":         status.Name,
		"status":         status.Status,
		"server_info":    status.ServerInfo,
		"tool_count":     status.ToolCount,
		"resource_count": status.ResourceCount,
	}
	out, _ := json.MarshalIndent(result, "", "  ")
	return string(out), nil
}
